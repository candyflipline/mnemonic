package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
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
	listen := fs.String("listen", "127.0.0.1:7100", "HTTP listen address")
	grantTTL := fs.Duration("grant-ttl", 10*time.Minute, "lifetime of a presigned part URL")
	deleteSlack := fs.Duration("delete-slack", time.Minute, "added to -grant-ttl and -max-call before a retired object may be deleted")
	maxCall := fs.Duration("max-call", 30*time.Second, "longest a call may run; added to -grant-ttl and -delete-slack before a retired object may be deleted")
	quiet := fs.Duration("quiet", 15*time.Minute, "a source settles into its cold part once its updated_ms is this far behind the newest source's")
	settle := fs.Bool("settle", false, "settle every source now")
	interval := fs.Duration("interval", 2*time.Second, "time between ingest cycles")
	fs.Parse(args)
	if *tenant == "" || strings.Contains(*tenant, "/") {
		return fmt.Errorf("keep: -tenant must be a non-empty name without /")
	}
	s, err := obj.Open(*storeURI)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	k, err := keeper.Take(ctx, s, keeper.Config{Tenant: *tenant, Root: *root, Local: *local, Quiet: *quiet, Settle: *settle, GrantTTL: *grantTTL, DeleteSlack: *deleteSlack, MaxCall: *maxCall})
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	ks := keepServer{k: k}
	return ks.serve(ctx, l, *interval, k.Cycle, make(chan error, 3))
}

// serve answers from the head Take installed while first, the startup cycle,
// runs beside the k.Run loop, which keeps the gen checked meanwhile. An error
// of first ends it; a clean first does not.
func (ks keepServer) serve(ctx context.Context, l net.Listener, interval time.Duration, first func(context.Context) error, errc chan error) error {
	srv := &http.Server{Handler: ks.mux()}
	go func() { errc <- srv.Serve(l) }()
	go func() {
		if err := first(ctx); err != nil {
			errc <- err
		}
	}()
	go func() { errc <- ks.k.Run(ctx, interval) }()
	err := <-errc
	exitIfLost(ks.k, err)
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

type keepServer struct {
	k *keeper.Keeper
}

type ingestReply struct {
	Gen       keeper.Gen `json:"gen"`
	Seq       keeper.Seq `json:"seq"`
	Parts     int        `json:"parts"`
	Unchanged int        `json:"unchanged"`
	Resumed   int        `json:"resumed"`
	Fresh     int        `json:"fresh"`
	Reparsed  int        `json:"reparsed"`
}

func (ks keepServer) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/head", ks.serveHead)
	m.HandleFunc("POST /v1/call", ks.serveCall)
	m.HandleFunc("POST /v1/ingest", ks.serveIngest)
	return m
}

func (ks keepServer) serveHead(w http.ResponseWriter, r *http.Request) {
	var kh route.KeeperHead
	var committed bool
	err := ks.k.Read(func(h keeper.Head) error {
		if committed = h.Manifest.Gen == ks.k.Gen(); committed {
			kh = route.KeeperHead{Gen: h.Manifest.Gen, Seq: h.Manifest.Seq, Hot: h.Hot}
		}
		return nil
	})
	switch {
	case err != nil:
		ks.refuse(w, err)
	case !committed:
		http.Error(w, fmt.Sprintf("keeper: no commit at gen %d yet", ks.k.Gen()), http.StatusServiceUnavailable)
	default:
		writeJSON(w, kh)
	}
}

func (ks keepServer) serveCall(w http.ResponseWriter, r *http.Request) {
	err := ks.k.Read(func(h keeper.Head) error {
		callHandler(h).ServeHTTP(w, r)
		return nil
	})
	if err != nil {
		ks.refuse(w, err)
	}
}

func callHandler(h keeper.Head) http.Handler {
	return reader.Serve(reader.Local(func() map[string]*trace.Part { return h.Parts }))
}

func (ks keepServer) serveIngest(w http.ResponseWriter, r *http.Request) {
	err := ks.k.Cycle(r.Context())
	var rep ingestReply
	if err == nil {
		err = ks.k.Read(func(h keeper.Head) error {
			t := ks.k.Totals()
			rep = ingestReply{Gen: h.Manifest.Gen, Seq: h.Manifest.Seq, Parts: len(h.Manifest.Parts) + len(h.Hot),
				Unchanged: t[trace.Unchanged], Resumed: t[trace.Resumed], Fresh: t[trace.Fresh], Reparsed: t[trace.Reparsed]}
			return nil
		})
	}
	if err != nil {
		ks.fail(w, err)
		return
	}
	writeJSON(w, rep)
}

// refuse answers a Read the gate turned away: 503 while the gen is unchecked,
// the exit of a lost keeper otherwise.
func (ks keepServer) refuse(w http.ResponseWriter, err error) {
	if errors.Is(err, keeper.ErrUnchecked) {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	ks.fail(w, err)
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
