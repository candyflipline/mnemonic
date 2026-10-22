package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/candyflipline/mnemonic/arena/keeper"
	"github.com/candyflipline/mnemonic/arena/obj"
	"github.com/candyflipline/mnemonic/arena/reader"
	"github.com/candyflipline/mnemonic/arena/route"
	"github.com/candyflipline/mnemonic/arena/trace"
)

const lostStatus = 3

func runKeep(args []string) error {
	fs := flag.NewFlagSet("keep", flag.ExitOnError)
	tenant := fs.String("tenant", "", "tenant")
	root := fs.String("root", "", "data root whose raw/ is the input")
	local := fs.String("local", "", "local trace store dir")
	storeURI := fs.String("store", "", "object store URI")
	readersFlag := fs.String("readers", "", "id=http://host:port,... reader servers for the partial rung")
	listen := fs.String("listen", "127.0.0.1:7100", "HTTP listen address")
	grantTTL := fs.Duration("grant-ttl", 10*time.Minute, "lifetime of a presigned part URL")
	quiet := fs.Duration("quiet", 15*time.Minute, "a source settles into its cold part once its updated_ms is this far behind the newest source's")
	settle := fs.Bool("settle", false, "settle every source now")
	interval := fs.Duration("interval", 2*time.Second, "time between ingest cycles")
	fs.Parse(args)
	if *tenant == "" || strings.Contains(*tenant, "/") {
		return fmt.Errorf("keep: -tenant must be a non-empty name without /")
	}
	readers, err := parseReaders(*readersFlag)
	if err != nil {
		return err
	}
	s, err := obj.Open(*storeURI)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 3)
	go func() {
		if err := obj.Serve(ctx, s); err != nil {
			errc <- err
		}
	}()
	if err := awaitServe(ctx, *storeURI, errc); err != nil {
		return err
	}
	k, err := keeper.Take(ctx, s, keeper.Config{Tenant: *tenant, Root: *root, Local: *local, Quiet: *quiet, Settle: *settle, GrantTTL: *grantTTL})
	if err != nil {
		return err
	}
	if err := k.Cycle(ctx); err != nil {
		exitIfLost(k, err)
		return err
	}
	ks := keepServer{k: k, s: s, readers: readers, ttl: *grantTTL}
	srv := &http.Server{Addr: *listen, Handler: ks.mux()}
	go func() { errc <- srv.ListenAndServe() }()
	go func() { errc <- k.Run(ctx, *interval) }()
	err = <-errc
	exitIfLost(k, err)
	srv.Close()
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func exitIfLost(k *keeper.Keeper, err error) {
	if errors.Is(err, keeper.ErrLost) {
		fmt.Fprintf(os.Stderr, "keep: gen %d lost to a newer keeper\n", k.Gen())
		os.Exit(lostStatus)
	}
}

func parseReaders(flag string) ([]route.Reader, error) {
	var readers []route.Reader
	for _, kv := range strings.Split(flag, ",") {
		if kv == "" {
			continue
		}
		id, u, ok := strings.Cut(kv, "=")
		if !ok || id == "" || id == route.LocalID || u == "" {
			return nil, fmt.Errorf("keep: bad -readers entry %q", kv)
		}
		readers = append(readers, route.HTTPReader(id, u))
	}
	return readers, nil
}

// awaitServe waits for a file store's serve= address to accept, since Latest
// reads manifests through it.
func awaitServe(ctx context.Context, storeURI string, errc chan error) error {
	u, err := url.Parse(storeURI)
	if err != nil {
		return err
	}
	serve := u.Query().Get("serve")
	if serve == "" {
		return nil
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(strings.TrimSuffix(serve, "/") + "/stats")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("%s", resp.Status)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("keep: store serve %s: %w", serve, err)
		}
		select {
		case err := <-errc:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

type keepServer struct {
	k       *keeper.Keeper
	s       obj.Store
	readers []route.Reader
	ttl     time.Duration
}

type callBody struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
	Rung string          `json:"rung"`
}

type callReply struct {
	Data  json.RawMessage `json:"data"`
	Text  string          `json:"text"`
	Error string          `json:"error"`
}

type ingestReply struct {
	Gen   keeper.Gen `json:"gen"`
	Seq   keeper.Seq `json:"seq"`
	Parts int        `json:"parts"`
}

var rungs = map[string]route.Rung{"whole": route.Whole, "partial": route.Partial}

func (ks keepServer) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/view", ks.serveView)
	m.HandleFunc("POST /v1/call", ks.serveCall)
	m.HandleFunc("POST /v1/ingest", ks.serveIngest)
	return m
}

func (ks keepServer) view(h keeper.Head) (route.View, error) {
	v := route.View{Tenant: h.Manifest.Tenant, Gen: h.Manifest.Gen, Seq: h.Manifest.Seq, Parts: make([]route.GrantedPart, 0, len(h.Manifest.Parts)+len(h.Hot))}
	for _, p := range h.Manifest.Parts {
		u, err := ks.s.PresignGet(p.Key, ks.ttl)
		if err != nil {
			return v, err
		}
		v.Parts = append(v.Parts, route.GrantedPart{PartRef: p, URL: u})
	}
	for _, p := range h.Hot {
		v.Parts = append(v.Parts, route.GrantedPart{PartRef: p, Local: true})
	}
	return v, nil
}

func (ks keepServer) serveView(w http.ResponseWriter, r *http.Request) {
	if after := r.URL.Query().Get("after"); after != "" {
		gs, ss, ok := strings.Cut(after, "-")
		g, err1 := strconv.ParseUint(gs, 16, 64)
		s, err2 := strconv.ParseUint(ss, 16, 64)
		if !ok || len(gs) != 16 || len(ss) != 16 || err1 != nil || err2 != nil {
			http.Error(w, "after must be %016x-%016x gen-seq", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		newer := ks.k.Wait(ctx, keeper.Gen(g), keeper.Seq(s))
		cancel()
		if !newer {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	var v route.View
	err := ks.k.Read(func(h keeper.Head) (err error) {
		v, err = ks.view(h)
		return err
	})
	if err != nil {
		ks.fail(w, err)
		return
	}
	writeJSON(w, v)
}

func (ks keepServer) serveCall(w http.ResponseWriter, r *http.Request) {
	var body callBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rung, ok := rungs[body.Rung]
	if !ok {
		http.Error(w, `rung must be "whole" or "partial"`, http.StatusBadRequest)
		return
	}
	var rep callReply
	var err error
	if body.Tool == "get_ingest_status" {
		rep.Data, rep.Text, err = ks.k.IngestStatus(body.Args)
	} else {
		err = ks.k.Read(func(h keeper.Head) error {
			v, err := ks.view(h)
			if err != nil {
				return err
			}
			readers := []route.Reader{reader.Local(func() map[string]*trace.Part { return h.Parts })}
			if rung == route.Partial {
				readers = append(readers, ks.readers...)
			}
			rep.Data, rep.Text, err = route.Do(r.Context(), v, readers, rung, body.Tool, body.Args)
			return err
		})
	}
	if errors.Is(err, keeper.ErrLost) {
		ks.fail(w, err)
		return
	}
	if err != nil {
		rep.Error = err.Error()
	}
	writeJSON(w, rep)
}

func (ks keepServer) serveIngest(w http.ResponseWriter, r *http.Request) {
	err := ks.k.Cycle(r.Context())
	var rep ingestReply
	if err == nil {
		err = ks.k.Read(func(h keeper.Head) error {
			rep = ingestReply{Gen: h.Manifest.Gen, Seq: h.Manifest.Seq, Parts: len(h.Manifest.Parts) + len(h.Hot)}
			return nil
		})
	}
	if err != nil {
		ks.fail(w, err)
		return
	}
	writeJSON(w, rep)
}

func (ks keepServer) fail(w http.ResponseWriter, err error) {
	exitIfLost(ks.k, err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}
