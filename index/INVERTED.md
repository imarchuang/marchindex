# How the inverted index works

marchindex answers a query from postings lists, not by scanning documents.
This note follows one example through analyze, postings, boolean combine,
flush, delete, and merge.

## Documents

```text
doc 0  {"service":"api","level":"error","message":"timeout calling db"}
doc 1  {"service":"api","level":"info", "message":"request ok"}
```

DocIDs are local to one segment and start at 0. A hit reports `_seg` and `_doc`
because the same number in another segment is a different document.

## Analyze

Each field except `_id` is lowercased and split on everything that is not a
letter or digit. Tokens shorter than 2 characters are not indexed, but they
still consume a position so a gap remains for phrase queries.

```text
message of doc 0:
  timeout@0  calling@1  db@2

message of "timeout a db":
  timeout@0  a@1 (dropped)  db@2
```

`"timeout db"` matches the second text only when `db` sits at `timeout`'s
position plus 1. Here it sits at plus 2, so the phrase misses.

Terms are stored as `field:term`:

```text
level:error      → doc 0 at [0]
level:info       → doc 1 at [0]
service:api      → doc 0 at [0], doc 1 at [0]
message:timeout  → doc 0 at [0]
message:calling  → doc 0 at [1]
message:db       → doc 0 at [2]
message:request  → doc 1 at [0]
message:ok       → doc 1 at [1]
```

## Boolean combine

`level:error AND service:api` looks up two posting lists and intersects the
docIDs. Both lists are sorted, so the intersection is a two-pointer walk:

```text
level:error  [0]
service:api  [0, 1]
intersection [0]
```

`postings_lookups` counts those dictionary lookups. It grows with the number of
query terms and segments, not with the number of documents. `docs_examined`
counts stored documents fetched after the docID list is known. The original
JSON for doc 0 is then read from the doc store.

`OR` is the same walk, keeping every docID from either list. `AND` binds
tighter than `OR`. A phrase is a single leaf: the doc must contain each token
at `firstPosition + delta`.

## On disk

A flush freezes the RAM buffer into `segments/seg-NNNNNN/` and only then
replaces `segments.json`. Search reads a segment only when the commit point
lists it.

`terms.bin` is the dictionary, sorted, one variable-length record per term:

```text
nameLen | name | postOff | postLen | df
```

`nameLen` is how many bytes of the name follow, because names differ in length.
`postOff` and `postLen` select that term's blob inside `postings.bin`. `df` is
how many documents contain the term.

`postings.bin` does not store the term text. After an 8-byte header, each blob is:

```text
count
repeated per document:
  docDelta | freq | pos0 | posDelta ...
```

`service:api` for the two documents above is the bytes `02 00 01 00 01 01 00`:

```text
2        two documents
0        docID 0
1  0     one position, value 0
1        docID 0+1
1  0     one position, value 0
```

`docs.bin` holds the original field JSON. `docs.idx` maps each local docID to
the byte offset of that record.

## Selective search reads

Search uses a request-scoped `segmentReader`, not the eager `openSegment`
used by merge/delete. The on-disk format is unchanged.

1. Read `segments.json`, segment metadata, `terms.bin`, `docs.idx`, deletion
   bits and the small postings/docs headers. Validate counts and offsets.
2. Resolve exact/phrase query terms in the dictionary. Fuzzy queries scan
   dictionary keys and select only terms within the requested edit distance.
3. Use `ReadAt(postOff, postLen)` for each distinct selected postings list.
   Decode it once per segment per request; boolean evaluation and TF scoring
   reuse it. An absent term reads no postings payload.
4. Combine docIDs, filter deleted documents, and apply ordering and `limit`.
5. Fetch only returned documents with `ReadAt`. A record ends at the next
   `docs.idx` offset (or EOF for the last document). No document payload is
   read for a miss or `limit=0`, although query postings are still evaluated.

Each response includes an `io` object:

- `metadata_bytes_read`: commit point, metadata, dictionary, offsets, deletion
  bits and file headers read during this request.
- `postings_bytes_read`: selected compressed postings payload bytes.
- `docs_bytes_read`: selected stored records, including their length prefixes.
- `postings_decoded`: distinct postings lists decoded; repeated query terms
  do not cause repeated reads. Unlike `postings_lookups`, this counts physical
  list decodes rather than logical query lookups.

Byte counters measure bytes returned by application file reads, **not physical
storage I/O**: the OS page cache may serve them. `docs_examined` still counts
returned documents, including RAM hits; it is not an I/O metric.

Readers close their file handles on success and error, before releasing the
index read lock. Flush/merge/delete retain the existing exclusive lock, so
readers cannot race segment removal or deletion-bit replacement. No reader
cache survives the request: each search still loads the full dictionary and
current deletion bits. This avoids stale readers, but metadata caching and
non-blocking merge remain future optimizations. Unselected payload corruption
is discovered only when that payload is read (or during eager maintenance).

## Delete

`DELETE` does not rewrite postings. It sets a bit in `deleted.bits` for that
local docID. Search still reads the posting list, then skips docIDs whose bit
is set. A copy that is still only in RAM is dropped immediately. The same `_id`
can live in more than one segment; each live copy gets a bit.

## Merge

Merge reads every committed segment, skips deleted docIDs, and assigns new
local docIDs in commit order. Positions are copied unchanged. Posting lists
from a later segment receive higher docIDs, so appending them stays sorted.

The new segment directory is finished before `segments.json` is replaced.
Until that replace, readers still see the old segments. Input directories are
removed only after the replace. A flush that reaches three committed segments
runs the same merge; `POST /_forcemerge` runs it as soon as two segments exist.

## Term frequency

`sort=tf` does not change which documents match. It orders the matches by the
sum of the position counts of the query terms. `"timeout timeout"` scores 2 for
the term `timeout`. Ties keep the original doc order. The default search stays
in doc order and does not return `_score`.
