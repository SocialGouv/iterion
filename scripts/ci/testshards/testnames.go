package main

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/testinventory"
)

// runTests divides a package without dividing a top-level test's subtests.
// Discovery uses Go's own inventory with the caller's build environment, so
// build constraints and examples agree with the subsequent go test command.
func runTests(n, i int, all bool, pkg string, timeout time.Duration, out io.Writer) error {
	if n < 1 || i >= n || timeout < 0 || strings.HasPrefix(pkg, "-") {
		return fail(2, "invalid test partition: -n >= 1, -i < n, -list-timeout >= 0 and one package are required")
	}
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	names, err := testinventory.Read(ctx, pkg)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("test discovery exceeded -list-timeout %s: %w", timeout, err)
		}
		return err
	}
	return emitTestPatterns(n, i, all, names, out)
}

func emitTestPatterns(n, i int, all bool, names []string, out io.Writer) error {
	if n < 1 || i >= n {
		return fail(2, "invalid test shard %d of %d", i, n)
	}
	shards := shardByHash(names, n)
	for index, shard := range shards {
		if len(shard) == 0 {
			return fail(1, "test shard %d of %d is empty over %d names; reduce -n", index, n, len(names))
		}
	}
	for index, shard := range shards {
		if !all && i >= 0 && index != i {
			continue
		}
		if all || i < 0 {
			if _, err := fmt.Fprintf(out, "# shard %d: %d top-level tests\n", index, len(shard)); err != nil {
				return err
			}
		}
		escaped := make([]string, len(shard))
		for j, name := range shard {
			escaped[j] = regexp.QuoteMeta(name)
		}
		if _, err := fmt.Fprintf(out, "^(%s)$\n", strings.Join(escaped, "|")); err != nil {
			return err
		}
	}
	return nil
}
