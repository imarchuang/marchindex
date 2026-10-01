# marchindex — Lucene / Elasticsearch-inspired inverted index MVP

Educational search engine core in Go. Same spirit as [marchilogs](../marchilogs)
and [marchiq](../marchiq): **one learning goal per slice**, inspectable on-disk
layout, HTTP-first API, Docker runnable, tests that prove the core loop.

**Not an Elasticsearch clone.** We borrow Lucene’s **inverted index + immutable
segments + merge** model — not cluster shards, replicas, Query DSL, aggregations,
or mapping APIs.

---

## Learning goal

After the MVP you can explain, with a running binary and on-disk files:

1. What a **true inverted index** is: `term → postings (docIDs [+ positions])`,
   vs marchilogs’ `tag → parts` / bloom prune.
2. How **analyze → index → boolean retrieve** works end-to-end.
3. Why **segments are immutable**, and why **merge** exists (LSM-flavored lifecycle
   without being a KV store). Specifically explain the immutability benefits:
   - Lock-free concurrent reads (readers never block writers).
   - Cache-friendly / OS page cache reuse (immutable files stay hot in cache).
   - Compression-friendly (delta/varint on sorted postings).
   - Crash safety via atomic commit-point swap (`segments.json`).
   - Merge = compaction that reclaims deleted docs.
4. How **delete** works with a bitset / tombstone until merge drops docs.

**Pass bar:** hand-write a term→postings merge; explain why immutable segments help
concurrency and compression. Do **not** stop at “bloom that skips blocks.”

---

## ES / Lucene concepts we keep (and what we drop)

| Lucene / ES idea | marchindex v0 | Deferred |
|---|---|---|
| Index | named collection of docs | multi-index aliases, ILM |
| Document | JSON flat string fields + `_id` | nested / object trees |
| Analyzer | lowercase + whitespace (or simple unicode split) | stemmers, ICU, synonyms |
| Inverted index | term → sorted unique docIDs | skip lists, roaring, block encodings |
| Positions | optional (phrase needs them) | payloads, offsets for highlighting |
| Segment | immutable unit of index files | Lucene codec compatibility |
| Commit point | `segments.json` lists active segments | generation / rollback history |
| Merge | background small→big segment merge | tiered policy tuning, force_merge HTTP |
| Delete | per-segment deleted bitset | soft deletes with retention lease |
| Near-real-time | RAM buffer → flush new segment | refresh interval semantics à la ES |
| Shard / replica | single process, one shard | cluster, routing, rebalance |
| `_source` vs index | doc store (`docs.bin`/`docs.idx`) vs inverted index (`terms.bin`/`postings.bin`); query phase → fetch phase | included fields (in-index fields to skip fetch phase) |

**`_source` vs Lucene index separation:**
ES separates `_source` (doc store) from the Lucene index, enabling a two-phase search — **query phase** (hit Lucene index to find docIDs) then **fetch phase** (enrich from `_source`). This is exactly marchindex's `docs.bin`/`docs.idx` vs `terms.bin`/`postings.bin` split, and the search path in the "Query model" section already follows query→fetch. In ES, the ideal optimization is pulling needed fields into the index (included fields) so fetch can be skipped entirely — marked as deferred for marchindex.

**Explicit non-goals (v0):**

- Distributed shards, replicas, rebalancing, master election
- Full Lucene codec / ES wire protocol / Query DSL
- Aggregations, KNN, geo, completion suggester
- Mapping types, dynamic templates, analyzers zoo
- Relevance scoring beyond optional TF (default: boolean match + doc order)

---

## Why this is not marchilogs indexdb

| | marchilogs | marchindex |
|---|---|---|
| Purpose | prune log **parts** before scan | find **documents** by terms |
| Index shape | stream tag → part ids | term → docIDs (+ optional positions) |
| Query | time + stream + contains (scan/bloom) | boolean over postings |
| Unit of merge | columnar parts | inverted segments |
| Learning target | VL-style log store | Lucene-style search core |

Analogy from the systems roadmap: *marchilogs indexdb is tag→part; here you build
term→docID(+pos), close to Lucene.*

---

## Core loop

```text
Indexer                              Engine (single node)                    Searcher
   |                                        |                                    |
   |  POST /indices/{idx}/_doc              |                                    |
   |  { "service":"api", "level":"error",   |                                    |
   |    "message":"timeout calling db" }    |                                    |
   +--------------------------------------->|  analyze fields → terms            |
   |                                        |  add to RAM buffer (in-memory      |
   |                                        |  inverted + doc store)             |
   |                                        |  return { _id, _seq }              |
   |                                        |                                    |
   |  POST /indices/{idx}/_flush            |                                    |
   +--------------------------------------->|  freeze buffer → write segment     |
   |                                        |  update segments.json (commit)     |
   |                                        |                                    |
   |                                        |  GET /indices/{idx}/_search?q=...  |
   |                                        |<-----------------------------------+
   |                                        |  load active segments              |
   |                                        |  lookup terms → intersect/union    |
   |                                        |  postings; skip deleted; fetch docs|
   |                                        +----------------------------------->|
   |                                        |                                    |
   |                                        |  (background) merge small segments |
```

---

## Data model

### Document (v0)

```json
{
  "_id": "auto-or-client",
  "service": "api",
  "level": "error",
  "message": "timeout calling db"
}
```

- All values treated as **text** for indexing (no typed numerics in v0).
- `_id` is string; if omitted, assign monotonic / ULID.
- Stored fields: keep original JSON (or field map) in **doc store** for fetch-by-id
  and search hits.

### Analysis (v0)

Per indexed field (default: all fields except `_id`):

1. Lowercase
2. Split on non-alphanumeric (whitespace + punctuation)
3. Drop empty tokens; optional min length 2

Field-scoped terms: store as `field:term` (e.g. `level:error`, `message:timeout`)
so queries can be field-aware without a separate per-field FST in v0.

### Inverted structure (in memory / on disk)

```text
term → [docID₀, docID₁, ...]   # strictly increasing
```

Optional for phrase (slice 4+):

```text
term → [ (docID, [pos₀, pos₁, ...]), ... ]
```

Postings on disk: delta-encode docIDs (varint) is enough for MVP; no roaring required.

---

## Storage model (immutable segments)

```text
{dataDir}/
  indices/
    {index}/
      segments.json              # commit point: active segment ids + gens
      segments/
        seg-000001/
          meta.json              # doc count, term count, created_ns
          terms.bin              # sorted terms + offsets into postings
          postings.bin           # delta-encoded docID lists
          docs.bin               # doc store: docID → stored JSON (or offsets)
          docs.idx               # sparse: docID → byte offset in docs.bin
          deleted.bits           # optional bitset; absent = none deleted
        seg-000002/
          ...
      buffer/                    # optional crash recovery of unflushed RAM (later)
```

**Commit rule:** readers only see segments listed in `segments.json`. Flush writes
the segment directory first, then atomically renames/replaces `segments.json`
(same idea as marchilogs manifest).

**Merge:** pick N small segments → rewrite one larger segment (merge-sort terms,
merge postings, drop docs marked deleted) → swap commit point → delete inputs.

This is **LSM-adjacent lifecycle** (immutable files + background merge) but the
payload is **inverted postings**, not KV SSTables.

**Real Lucene segment contents vs marchindex v0:**
- **Real Lucene segment contains:** inverted index (`term → docs`), stored fields (`_source`), term vectors (phrase/highlight), postings lists (`docIDs`, `positions`, `frequencies`), norms (scoring), and doc values (sorting/faceting).
- **marchindex v0 keeps:** inverted index + postings + stored fields (`terms.bin`, `postings.bin`, `docs.bin`/`docs.idx`). Positions are kept only from slice 4 (for phrase queries); norms, doc values, and term vectors are dropped.

---

## Query model (v0 boolean)

Minimal query language (query string, not ES DSL):

| Query | Meaning |
|---|---|
| `level:error` | term in field `level` |
| `timeout` | term in default field(s), e.g. `message` |
| `a AND b` | intersection of postings |
| `a OR b` | union of postings |
| `NOT a` | (optional) universe minus postings — defer if hard; prefer `-term` on default field |
| `"timeout db"` | phrase (needs positions; slice 4) |

**Search path:**

1. Parse query → AST of term leaves + AND/OR
2. For each active segment: lookup terms → postings → boolean combine → apply deleted bitset
3. Merge hit docIDs across segments (docIDs are **per-segment** in v0; return
   `{segment, localDocID}` or assign global docID at index time — pick one and stick to it)
4. Fetch stored docs; return hits + `docs_examined` / `postings_lookups` stats

**Recommended docID scheme (v0):** per-segment local docIDs starting at 0; hits
carry `seg` + `doc`. Simpler than global ID remapping on merge.

---

## API (HTTP)

| Method | Path | Purpose |
|---|---|---|
| GET | `/healthz` | liveness |
| PUT | `/indices/{name}` | create index |
| GET | `/indices` | list indices + segment counts |
| POST | `/indices/{name}/_doc` | index one document (body JSON) |
| POST | `/indices/{name}/_bulk` | NDJSON bulk (optional polish) |
| POST | `/indices/{name}/_flush` | RAM → new segment + commit |
| DELETE | `/indices/{name}/_doc/{id}` | mark deleted (bitset) |
| GET | `/indices/{name}/_search` | `q=`, `limit=` |
| POST | `/indices/{name}/_forcemerge` | optional; compact segments now |
| GET | `/indices/{name}/_stats` | docs, segments, terms, deletes |
| GET | `/` | help text |

Default listen `:9200` (nod to ES) or `:8080` — either is fine; document in flags.

---

## MVP slices (build order)

Each slice = branch + tests + still Docker-runnable.

### Slice 0 — skeleton

- `go mod`, `cmd/marchindex/main.go`, `index/` package
- `-dataDir`, `-addr`, `GET /healthz`, `GET /`
- Create empty index directory

**Done when:** `go test ./...` green, Docker image builds.

### Slice 1 — in-memory inverted index + boolean search

- Analyzer + RAM `term → []docID` + doc store
- Index docs via HTTP; search `AND` / `OR` without flush
- Stats: `docs_examined` vs naive full scan baseline in a test

**Tests:** index N docs; `level:error AND service:api` returns expected IDs;
compare postings path vs scanning all docs.

### Slice 2 — flush segment + commit point

- Freeze RAM buffer → write `terms.bin` / `postings.bin` / `docs.*`
- `segments.json` atomic update
- Search reads **only** committed segments (+ optional still-open RAM buffer for NRT)

**Tests:** flush, process restart, same query hits; inspect files on disk.

### Slice 3 — merge + delete bitset

- Background or forced merge of ≥K small segments
- `DELETE _doc/{id}` sets bit in owning segment; search skips; merge drops
- Commit swap never exposes half-merged output

**Tests:** create many tiny segments → merge → count drops; deleted doc gone after merge.

### Slice 4 — phrase (positions) — optional but high learning value

- Store positions in postings
- `"foo bar"` requires adjacent positions in same doc

**Tests:** phrase hits only when terms are consecutive.

### Slice 5 — polish

- Bulk index, `_stats`, forcemerge HTTP
- Simple scoring optional (term freq)
- `cmd/demo/` load corpus + query script
- Document `INVERTED.md` (how postings merge works)

---

## Observability (minimal)

| Signal | v0 |
|---|---|
| Log | index/flush/merge/search with counts |
| Search response | `hits`, `took_ms`, `segments_searched`, `postings_lookups` |
| Debug | `GET /indices/{name}/_stats` segment sizes + del counts |

---

## Project layout (target)

```text
marchindex/
  PLAN.md                 # this file
  go.mod
  Dockerfile
  docker-compose.yml
  cmd/
    marchindex/main.go
    demo/
  index/
    analyzer.go
    ram.go                # in-memory inverted + doc store
    segment.go            # read/write segment files
    commit.go             # segments.json
    merge.go
    search.go             # boolean over postings
    delete.go             # bitset
    INVERTED.md           # design notes (slice 2+)
  index/*_test.go
```

Module path: `github.com/marchi/marchindex`.

---

## Demo script (graduation bar)

```bash
docker compose up

curl -X PUT localhost:9200/indices/logs

curl -X POST localhost:9200/indices/logs/_doc \
  -H 'Content-Type: application/json' \
  -d '{"service":"api","level":"error","message":"timeout calling db"}'

curl -X POST localhost:9200/indices/logs/_doc \
  -H 'Content-Type: application/json' \
  -d '{"service":"api","level":"info","message":"request ok"}'

curl -X POST localhost:9200/indices/logs/_flush

curl 'localhost:9200/indices/logs/_search?q=level:error%20AND%20service:api'
# → 1 hit; response includes postings_lookups > 0

# After many flushes:
curl -X POST localhost:9200/indices/logs/_forcemerge
curl localhost:9200/indices/logs/_stats   # fewer segments
```

Pass: boolean query works after restart; merge reduces segment count; delete hides
docs; you can open `terms.bin` / `postings.bin` and explain the layout.

---

## Relation to sibling projects

| Project | Overlap | Difference |
|---|---|---|
| **marchilogs** | immutable parts, manifest, merge | columnar logs + bloom; not term→doc |
| **marchiq** | segment files, commit-style visibility | append log + consumer offset; not inverted |
| **marchindex** | segment + merge | **full inverted index** + boolean retrieval |
| **marchimetrics** | time parts / merge | numeric samples |

Place in the systems roadmap: after Kafka (marchiq), before Postgres txn —
*“补全真倒排”*.

---

## Traps to avoid

1. **Bloom-only and calling it inverted** — bloom answers “maybe in block”; inverted
   answers “which docIDs”.
2. **Mutating postings in place** — always write new segment + commit swap.
3. **Global docID churn on every merge** without a plan — prefer per-segment IDs.
4. **Skipping delete bitset** — then you cannot teach Lucene-style delete-before-merge.

---

## Open decisions (defaults for v0)

| Question | Default | Revisit when |
|---|---|---|
| DocID scope | per-segment local | need stable `_id` → seg mapping (hash map in meta) |
| Default search field | `message` | multi-field `query_string` |
| NRT | search RAM buffer + disk segments | ES-like refresh_interval |
| Encoding | sorted terms + varint deltas | roaring / FOR |
| Phrase | slice 4 | if boolean-only is enough for pass bar |

---

## Success criteria (“MVP done”)

- [ ] Index documents; flush to immutable segments; restart still serves search
- [ ] Boolean `AND` / `OR` over real postings (not full scan)
- [ ] Delete via bitset; merge drops deleted docs
- [ ] Stats show segments / postings work; demo script documented
- [ ] `INVERTED.md` explains analyze → postings → intersect → merge

---

## After MVP (not now)

1. BM25 scoring + norms
2. Multi-field mappings / keyword vs text
3. Single-node “shard” split by `_id` hash (teaching routing only)
4. Highlighting via offsets
5. Pagination trio (in increasing sophistication):
   - `from`/`size` offset paging: simple, but re-sorts whole dataset per request and can show duplicates when new docs arrive.
   - `search_after` keyset paging: avoids duplicates, but a doc updated mid-pagination can be missed.
   - PIT / cursor paging: MVCC-style snapshot at query time — no duplicates, no misses, highest overhead via `keep_alive`.
6. Nested vs separate index/doc decision rule: denormalize/nest when the sub-model is read-heavy and rarely updated (avoids joins); use a separate index/doc-collection when writes/updates are frequent relative to the parent model.

Start implementation at **slice 0** on branch `feat/skeleton`.
