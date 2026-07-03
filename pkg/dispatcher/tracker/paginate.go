package tracker

import (
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// paginateUntilShort drives the ListCandidates pattern shared by the
// gitlab and forgejo adapters: call fetchPage for page 1, 2, ... until it
// reports fewer than pageSize items fetched — the cheapest portable
// "no more pages" signal for providers that don't reliably ship a Link
// header — or until pageCap is hit (belt + suspenders against a
// pathological server that always returns exactly pageSize items).
func paginateUntilShort(pageSize, pageCap int, logger *iterlog.Logger, capWarning string, fetchPage func(page int) (int, error)) error {
	for page := 1; ; page++ {
		n, err := fetchPage(page)
		if err != nil {
			return err
		}
		if n < pageSize {
			return nil
		}
		if page >= pageCap {
			if logger != nil {
				logger.Warn("%s", capWarning)
			}
			return nil
		}
	}
}
