// Package keeper is the one writer per tenant: it claims a generation and
// commits manifests to the object store.
//
// Keys:
//
//	t/<tenant>/g/<%016x gen>
//	t/<tenant>/m/<%016x gen>-<%016x seq>.json
//	t/<tenant>/p/<id>.g<gen>
package keeper

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/arena/obj"
)

type Gen uint64

type Seq uint64

type PartRef struct {
	ID         string
	Gen        Gen
	Key        obj.Key
	Size       int64
	SHA256     string
	Sessions   []string
	ScoredDocs int64
	TotalLen   int64
}

type Manifest struct {
	Tenant  string
	Gen     Gen
	Seq     Seq
	Parts   []PartRef
	Sources json.RawMessage
}

func genPrefix(tenant string) obj.Key { return obj.Key("t/" + tenant + "/g/") }

func manifestPrefix(tenant string) obj.Key { return obj.Key("t/" + tenant + "/m/") }

func partPrefix(tenant string) obj.Key { return obj.Key("t/" + tenant + "/p/") }

func genKey(tenant string, g Gen) obj.Key {
	return obj.Key(fmt.Sprintf("%s%016x", genPrefix(tenant), uint64(g)))
}

func manifestKey(tenant string, g Gen, s Seq) obj.Key {
	return obj.Key(fmt.Sprintf("%s%016x-%016x.json", manifestPrefix(tenant), uint64(g), uint64(s)))
}

func partKey(tenant, id string, g Gen) obj.Key {
	return obj.Key(fmt.Sprintf("%s%s.g%d", partPrefix(tenant), id, uint64(g)))
}

func parseGenKey(tenant string, k obj.Key) (Gen, error) {
	g, err := strconv.ParseUint(strings.TrimPrefix(string(k), string(genPrefix(tenant))), 16, 64)
	if err != nil {
		return 0, fmt.Errorf("keeper: bad gen key %s", k)
	}
	return Gen(g), nil
}

func parseManifestKey(tenant string, k obj.Key) (Gen, Seq, error) {
	name, ok := strings.CutSuffix(strings.TrimPrefix(string(k), string(manifestPrefix(tenant))), ".json")
	gs, ss, ok2 := strings.Cut(name, "-")
	if !ok || !ok2 || len(gs) != 16 || len(ss) != 16 {
		return 0, 0, fmt.Errorf("keeper: bad manifest key %s", k)
	}
	g, err1 := strconv.ParseUint(gs, 16, 64)
	s, err2 := strconv.ParseUint(ss, 16, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("keeper: bad manifest key %s", k)
	}
	return Gen(g), Seq(s), nil
}

// keyGen is the gen a manifest or part key was written at.
func keyGen(tenant string, k obj.Key) (Gen, bool) {
	if strings.HasPrefix(string(k), string(manifestPrefix(tenant))) {
		g, _, err := parseManifestKey(tenant, k)
		return g, err == nil
	}
	name, ok := strings.CutPrefix(string(k), string(partPrefix(tenant)))
	i := strings.LastIndex(name, ".g")
	if !ok || i < 0 {
		return 0, false
	}
	g, err := strconv.ParseUint(name[i+2:], 10, 64)
	return Gen(g), err == nil
}

func highestGen(ctx context.Context, s obj.Store, tenant string) (Gen, error) {
	es, err := s.List(ctx, genPrefix(tenant))
	if err != nil {
		return 0, err
	}
	var top Gen
	for _, e := range es {
		g, err := parseGenKey(tenant, e.Key)
		if err != nil {
			return 0, err
		}
		top = max(top, g)
	}
	return top, nil
}

// Latest reads the manifest with the highest (gen, seq); ok is false when the
// tenant has none.
func Latest(ctx context.Context, s obj.Store, tenant string) (m Manifest, ok bool, err error) {
	es, err := s.List(ctx, manifestPrefix(tenant))
	if err != nil {
		return m, false, err
	}
	var g Gen
	var q Seq
	var key obj.Key
	for _, e := range es {
		eg, eq, err := parseManifestKey(tenant, e.Key)
		if err != nil {
			return m, false, err
		}
		if key == "" || eg > g || (eg == g && eq > q) {
			g, q, key = eg, eq, e.Key
		}
	}
	if key == "" {
		return m, false, nil
	}
	b, err := get(ctx, s, key)
	if err != nil {
		return m, false, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, false, fmt.Errorf("keeper: %s: %w", key, err)
	}
	if m.Tenant != tenant || m.Gen != g || m.Seq != q {
		return m, false, fmt.Errorf("keeper: %s holds %s %d-%d", key, m.Tenant, m.Gen, m.Seq)
	}
	return m, true, nil
}

func get(ctx context.Context, s obj.Store, k obj.Key) ([]byte, error) {
	url, err := s.PresignGet(k, time.Minute)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("keeper: GET %s: %s", k, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// Claim creates g/<gen> exclusively for the next gen above every claimed one,
// re-listing and trying the next gen on ErrExists.
func Claim(ctx context.Context, s obj.Store, tenant string) (Gen, error) {
	for {
		top, err := highestGen(ctx, s, tenant)
		if err != nil {
			return 0, err
		}
		err = s.PutExclusive(ctx, genKey(tenant, top+1), bytes.NewReader(nil), 0)
		if errors.Is(err, obj.ErrExists) {
			continue
		}
		return top + 1, err
	}
}

var ErrNotExclusive = errors.New("keeper: the object store does not enforce exclusive create (If-None-Match: *)")

// probeExclusive creates a fresh key outside g/, m/ and p/ exclusively twice;
// the second create must fail with ErrExists.
func probeExclusive(ctx context.Context, s obj.Store, tenant string) error {
	key := obj.Key("t/" + tenant + "/x/probe-" + rand.Text())
	if err := s.PutExclusive(ctx, key, bytes.NewReader(nil), 0); err != nil {
		return err
	}
	err := s.PutExclusive(ctx, key, bytes.NewReader(nil), 0)
	switch {
	case errors.Is(err, obj.ErrExists):
		err = nil
	case err == nil:
		err = ErrNotExclusive
	}
	return errors.Join(err, s.Delete(ctx, []obj.Key{key}))
}

// Current reports whether gen is still the highest claimed gen.
func Current(ctx context.Context, s obj.Store, tenant string, gen Gen) (bool, error) {
	top, err := highestGen(ctx, s, tenant)
	return top == gen, err
}
