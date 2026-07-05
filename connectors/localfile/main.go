// localfile is the Phase-0 connector (spec §14.4): it walks a directory and
// posts every text file to POST /v1/evidence, idempotently (re-runs are
// cheap no-ops thanks to content-hash idempotency). Stateless beyond the
// filesystem itself.
//
//	localfile --base http://localhost:8080 --token $TOK --dir ./docs \
//	          --namespace /acme/team/repos/x --source-type doc --uri-prefix doc://x
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/lazorfuzz/memba/pkg/client"
	"github.com/lazorfuzz/memba/pkg/memory"
)

func main() {
	base := flag.String("base", "http://localhost:8080", "memd base URL")
	token := flag.String("token", os.Getenv("MEMBA_TOKEN"), "bearer token")
	dir := flag.String("dir", "", "directory to ingest")
	namespace := flag.String("namespace", "", "target namespace")
	sourceType := flag.String("source-type", memory.SourceDoc, "source type")
	uriPrefix := flag.String("uri-prefix", "", "source_uri prefix (default doc://<dirname>)")
	maxBytes := flag.Int64("max-bytes", 1<<20, "skip files larger than this")
	flag.Parse()
	if *dir == "" || *namespace == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "--dir, --namespace, --token required")
		os.Exit(2)
	}
	prefix := *uriPrefix
	if prefix == "" {
		prefix = "doc://" + filepath.Base(*dir)
	}
	c := client.New(*base, *token)
	ctx := context.Background()

	var ok, skipped, failed int
	err := filepath.WalkDir(*dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() == 0 || info.Size() > *maxBytes {
			skipped++
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil || !utf8.Valid(body) {
			skipped++
			return nil
		}
		rel, _ := filepath.Rel(*dir, path)
		mt := info.ModTime().UTC()
		_, err = c.InsertEvidence(ctx, memory.InsertRequest{
			NamespaceID: *namespace,
			SourceType:  *sourceType,
			SourceURI:   prefix + "/" + filepath.ToSlash(rel),
			Body:        string(body),
			Metadata:    map[string]any{"path": filepath.ToSlash(rel)},
			EventTime:   &mt,
		})
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "failed %s: %v\n", rel, err)
			return nil
		}
		ok++
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "walk:", err)
		os.Exit(1)
	}
	fmt.Printf("ingested %d files (%d skipped, %d failed) in %s\n", ok, skipped, failed, time.Now().Format(time.Kitchen))
	if failed > 0 {
		os.Exit(1)
	}
}
