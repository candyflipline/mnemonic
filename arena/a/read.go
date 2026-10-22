package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/candyflipline/mnemonic/arena/reader"
	"github.com/candyflipline/mnemonic/arena/route"
)

func runRead(args []string) error {
	fs := flag.NewFlagSet("read", flag.ExitOnError)
	id := fs.String("id", "", "this reader's ID, one of -readers")
	listen := fs.String("listen", "127.0.0.1:7101", "address for POST /v1/call")
	dir := fs.String("cache", "", "blob cache dir")
	keeper := fs.String("keeper", "", "keeper base URL")
	readers := fs.String("readers", "", "comma-separated reader IDs")
	fs.Parse(args)
	ids := strings.Split(*readers, ",")
	if *id == "" || *dir == "" || *keeper == "" || !contains(ids, *id) {
		return fmt.Errorf("read: -id, -cache, -keeper and -readers (naming -id) are required")
	}
	cache, err := reader.NewCache(*dir)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Handler: cache.Handler()}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()
	go prewarmLoop(ctx, cache, *keeper, *id, ids)
	log.Printf("read %s: listening on %s", *id, ln.Addr())
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// prewarmLoop long-polls the keeper's views and fetches the cold parts this
// reader owns as each one arrives.
func prewarmLoop(ctx context.Context, cache *reader.Cache, keeper, id string, ids []string) {
	after := ""
	for ctx.Err() == nil {
		v, err := nextView(ctx, keeper, after)
		if err != nil {
			log.Printf("read %s: view: %v", id, err)
			time.Sleep(time.Second)
			continue
		}
		if v == nil {
			continue
		}
		after = fmt.Sprintf("%016x-%016x", uint64(v.Gen), uint64(v.Seq))
		var own []route.GrantedPart
		for _, p := range v.Parts {
			if !p.Local && route.Owner(p.ID, ids)[0] == id {
				own = append(own, p)
			}
		}
		if err := cache.Prewarm(ctx, v.Tenant, own); err != nil {
			log.Printf("read %s: prewarm %s: %v", id, after, err)
		}
	}
}

func nextView(ctx context.Context, keeper, after string) (*route.View, error) {
	u := keeper + "/v1/view"
	if after != "" {
		u += "?after=" + url.QueryEscape(after)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusNoContent, http.StatusNotModified:
		return nil, nil
	case http.StatusOK:
		var v route.View
		return &v, json.NewDecoder(res.Body).Decode(&v)
	}
	return nil, fmt.Errorf("keeper: %s", res.Status)
}
