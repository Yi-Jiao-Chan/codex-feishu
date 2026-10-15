package attachments

import (
	"context"
	"errors"
	"sync"
	"time"

	"codex-feishu/internal/feishu"
	"codex-feishu/internal/store"
)

type Worker struct {
	Store         *store.Store
	Cache         *Cache
	Gateway       ResourceGateway
	HostID, AppID string
	Wake          func(string)
	Notice        func(string)
	Begin         func(context.Context, string) (context.Context, func(), error)
}

func (w *Worker) wake(thread string) {
	if w.Wake != nil && thread != "" {
		w.Wake(thread)
	}
}

func (w *Worker) Refresh(ctx context.Context) error {
	if err := w.Store.RecoverExpiredAttachmentDownloads(ctx, w.HostID, w.AppID, time.Now().UnixMilli()); err != nil {
		return err
	}
	threads, err := w.Store.RefreshAttachmentBatches(ctx, w.HostID, w.AppID, w.Cache.Limits.MaxBatchBytes, w.Cache.Limits.MaxImagesBytes, w.Cache.Resolve)
	for _, thread := range threads {
		w.wake(thread)
	}
	return err
}

func downloadFailure(err error) (string, bool) {
	var local *cacheError
	if errors.As(err, &local) {
		return local.code, local.code == "download-interrupted"
	}
	var rejected *feishu.Rejected
	if errors.As(err, &rejected) {
		return "download-rejected", rejected.HTTPStatus == 429 || rejected.HTTPStatus >= 500
	}
	return "download-interrupted", true
}

// ProcessOne is one bounded GET attempt; the receipt's native write is owned by
// the existing per-thread controller, never by a resource worker.
func (w *Worker) ProcessOne(ctx context.Context) (bool, error) {
	p, claimed, err := w.Store.ClaimAttachmentDownload(ctx, w.HostID, w.AppID, time.Now().UnixMilli())
	if err != nil || !claimed {
		return claimed, err
	}
	opctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	release := func() {}
	if w.Begin != nil {
		var beginErr error
		opctx, release, beginErr = w.Begin(opctx, p.Scope.ThreadID)
		if beginErr != nil || opctx == nil || release == nil {
			if release != nil {
				release()
			}
			_ = w.Store.ReleaseAttachmentDownload(ctx, p, time.Now().Add(5*time.Second).UnixMilli())
			return true, store.ErrIneligible
		}
	}
	defer release()
	if err := w.Refresh(opctx); err != nil {
		_ = w.Store.ReleaseAttachmentDownload(ctx, p, time.Now().Add(time.Second).UnixMilli())
		return true, err
	}
	w.wake(p.Scope.ThreadID)
	ready, getErr := w.Cache.Download(opctx, w.Gateway, p)
	code, retry := "", false
	if getErr != nil {
		code, retry = downloadFailure(getErr)
	} else {
		p = ready
	}
	// Persist the GET result even if shutdown cancelled its request. A failed
	// local save is recovered from the final file or retried GET on restart.
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer saveCancel()
	if err := w.Store.FinishAttachmentDownload(saveCtx, p, code, retry, time.Now().UnixMilli()); err != nil {
		_ = w.Store.ReleaseAttachmentDownload(saveCtx, p, time.Now().Add(time.Second).UnixMilli())
		return true, err
	}
	if err := w.Refresh(saveCtx); err != nil {
		return true, err
	}
	w.wake(p.Scope.ThreadID)
	return true, nil
}

func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < w.Cache.Limits.DownloadWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				worked, err := w.ProcessOne(ctx)
				if err != nil && w.Notice != nil {
					w.Notice("attachments.download_pending")
				}
				if worked && err == nil {
					continue
				}
				timer := time.NewTimer(500 * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	}
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	defer wg.Wait()
	for {
		if err := w.Refresh(ctx); err != nil && w.Notice != nil && ctx.Err() == nil {
			w.Notice("attachments.refresh_pending")
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}
