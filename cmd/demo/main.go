// Command demo loads a tiny log corpus, flushes it, and runs the queries
// from the slice-5 graduation script. It talks to the index package directly.
//
//	go run ./cmd/demo
package main

import (
	"fmt"
	"os"

	"github.com/imarchuang/marchindex/index"
)

func main() {
	dir, err := os.MkdirTemp("", "marchindex-demo-")
	if err != nil {
		fatal(err)
	}
	mgr, err := index.NewManager(dir)
	if err != nil {
		fatal(err)
	}
	idx, err := mgr.CreateIndex("logs")
	if err != nil {
		fatal(err)
	}

	corpus := []struct {
		id     string
		fields map[string]string
	}{
		{"", map[string]string{"service": "api", "level": "error", "message": "timeout calling db"}},
		{"", map[string]string{"service": "api", "level": "info", "message": "request ok"}},
		{"", map[string]string{"service": "worker", "level": "error", "message": "disk full"}},
		{"", map[string]string{"service": "api", "level": "error", "message": "timeout timeout calling db"}},
	}
	for _, doc := range corpus {
		res, err := idx.IndexDocument(doc.id, doc.fields)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("index _id=%s _seq=%d\n", res.ID, res.Seq)
	}
	flushed, err := idx.Flush()
	if err != nil {
		fatal(err)
	}
	fmt.Printf("flush segment=%s docs=%d\n", flushed.Segment, flushed.Docs)

	for _, q := range []string{
		"level:error AND service:api",
		`"timeout calling"`,
	} {
		res, err := idx.Search(q, 10)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("search %s hits=%d lookups=%d\n", q, len(res.Hits), res.PostingsLookups)
		for _, hit := range res.Hits {
			fmt.Printf("  _id=%s _seg=%s message=%s\n", hit["_id"], hit["_seg"], hit["message"])
		}
	}
	ranked, err := idx.SearchTF("timeout", 10)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("search timeout sort=tf\n")
	for _, hit := range ranked.Hits {
		fmt.Printf("  _id=%s _score=%s message=%s\n", hit["_id"], hit["_score"], hit["message"])
	}
	if len(ranked.Hits) < 2 || ranked.Hits[0]["_score"] != "2" {
		fatalf("expected the repeated timeout document first, got %#v", ranked.Hits)
	}
	st, err := idx.Stats()
	if err != nil {
		fatal(err)
	}
	fmt.Printf("stats docs=%d segments=%d dir=%s\n", st.Docs, len(st.Segments), dir)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
