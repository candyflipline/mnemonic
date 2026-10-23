package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/wire"
)

const (
	maxRootsBody    = 1 << 20
	backfillSources = 16
	backfillBytes   = 64 << 20
)

// rootsSync reports root facts ahead of each raw PUT. A server that answers
// 404 does not know the endpoint; the process then pushes raw alone.
type rootsSync struct {
	red       Redactor
	p         *prober
	off       bool
	unaligned map[string]int64
}

func newRootsSync(red Redactor) *rootsSync {
	return &rootsSync{red: red, p: newProber(red), unaligned: make(map[string]int64)}
}

func (r *rootsSync) newPass() { r.p.reset() }

// report posts the facts for body, whose first line starts at redacted offset from.
func (r *rootsSync) report(ctx context.Context, cfg config.Client, hc *http.Client, f fileRef, srcID string, from int64, body []byte) error {
	if r.off {
		return nil
	}
	_, err := r.post(ctx, cfg, hc, f, srcID, from, from+int64(len(body)), sniff(body, from, r.p))
	return err
}

// post sends facts as one or more POSTs with contiguous ranges covering
// [from, through); facts are sorted by From and a range is cut only where From
// changes, so each fact lies in its own range.
func (r *rootsSync) post(ctx context.Context, cfg config.Client, hc *http.Client, f fileRef, srcID string, from, through int64, facts []wire.Root) (now *wire.Source, err error) {
	if facts == nil {
		facts = []wire.Root{}
	}
	for lo := 0; ; {
		hi, next := roots1MiB(facts, lo)
		t := through
		if next < len(facts) {
			t = facts[next].From
		}
		s, err := r.postRange(ctx, cfg, hc, f, srcID, from, t, facts[lo:hi])
		if err != nil || r.off {
			return nil, err
		}
		now, from, lo = s, t, next
		if lo >= len(facts) {
			return now, nil
		}
	}
}

// roots1MiB returns the end of the group that starts at lo and where the next
// starts. Facts of one From that do not fit one POST are dropped past the limit.
func roots1MiB(facts []wire.Root, lo int) (hi, next int) {
	size, hi := 2, lo
	for hi < len(facts) && hi-lo < wire.MaxRootsPerPost {
		b, _ := json.Marshal(facts[hi])
		if size+len(b)+1 > maxRootsBody && hi > lo {
			break
		}
		size += len(b) + 1
		hi++
	}
	next = hi
	if hi < len(facts) && facts[hi].From == facts[hi-1].From {
		cut := hi
		for cut > lo && facts[cut].From == facts[cut-1].From {
			cut--
		}
		if cut > lo {
			hi, next = cut, cut
		} else {
			for next < len(facts) && facts[next].From == facts[lo].From {
				next++
			}
		}
	}
	return hi, next
}

func (r *rootsSync) postRange(ctx context.Context, cfg config.Client, hc *http.Client, f fileRef, srcID string, from, through int64, facts []wire.Root) (*wire.Source, error) {
	body, err := json.Marshal(facts)
	if err != nil {
		return nil, err
	}
	q := url.Values{"from": {strconv.FormatInt(from, 10)}, "through": {strconv.FormatInt(through, 10)}}
	u := strings.TrimSuffix(cfg.Server, "/") + wire.RootsPathPrefix +
		url.PathEscape(cfg.Machine) + "/" + url.PathEscape(f.harness) + "/" + srcID + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var s wire.Source
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&s); err != nil {
			return nil, fmt.Errorf("roots %s: 200 body: %w", srcID, err)
		}
		_, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return &s, err
	case http.StatusNotFound:
		r.off = true
		return nil, nil
	default:
		return nil, fmt.Errorf("roots %s: %s: %s", srcID, resp.Status, readErrBody(resp.Body))
	}
}

// backfillBudget bounds one pass; deferred says a source was left for the next.
type backfillBudget struct {
	sources  int
	bytes    int64
	deferred bool
}

func (b *backfillBudget) spent() bool {
	return b.sources >= backfillSources || b.bytes >= backfillBytes
}

// backfill reports the facts of [entry.RootsTo, entry.Size) for a source whose
// file is still on disk. A file whose redaction no longer lands on the server's
// Size is left alone until Size changes.
func (r *rootsSync) backfill(ctx context.Context, cfg config.Client, hc *http.Client, f fileRef, entry *wire.Source, b *backfillBudget) error {
	if r.off || entry == nil || entry.Size == 0 || entry.RootsTo >= entry.Size || r.unaligned[entry.SrcID] == entry.Size {
		return nil
	}
	if b.spent() {
		b.deferred = true
		return nil
	}
	file, err := redactComplete(f.path, r.red)
	if err != nil || file == nil {
		return nil
	}
	b.sources++
	b.bytes += int64(len(file.body))
	end, ok := cutAtSize(file.body, entry.Size)
	var start int64
	if ok && entry.RootsTo > 0 {
		start, ok = cutAtSize(file.body, entry.RootsTo)
	}
	if !ok || file.head != entry.HeadHash {
		r.unaligned[entry.SrcID] = entry.Size
		return nil
	}
	now, err := r.post(ctx, cfg, hc, f, entry.SrcID, entry.RootsTo, entry.Size, sniff(file.body[start:end], start, r.p))
	if now != nil {
		entry.RootsTo = now.RootsTo
	}
	return err
}
