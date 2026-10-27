package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/candyflipline/mnemonic/internal/qnet"
	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/candyflipline/mnemonic/internal/wire/pushpb"
)

type client struct {
	machine string
	conn    *grpc.ClientConn
	stream  pushpb.Push_SyncClient
	cancel  context.CancelFunc
	code    codes.Code
	acked   bool
	done    chan codes.Code
}

func open(server string, dial grpc.DialOption, machine string) (*client, error) {
	conn, err := grpc.NewClient("passthrough:///"+server, grpc.WithTransportCredentials(insecure.NewCredentials()), dial)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := pushpb.NewPushClient(conn).Sync(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	c := &client{machine: machine, conn: conn, stream: stream, cancel: cancel, code: codes.OK, done: make(chan codes.Code, 1)}
	if err := stream.Send(&pushpb.ClientMsg{Msg: &pushpb.ClientMsg_Hello{Hello: &pushpb.Hello{Machine: machine}}}); err != nil {
		c.code = status.Code(err)
	}
	return c, nil
}

func (c *client) welcome() {
	for c.code == codes.OK {
		msg, err := c.stream.Recv()
		if err != nil {
			c.code = status.Code(err)
			return
		}
		if w := msg.GetWelcome(); w != nil && !w.More {
			return
		}
	}
}

func (c *client) appendAck() {
	if c.code != codes.OK {
		return
	}
	path := "/capdrv/" + c.machine + ".jsonl"
	body := []byte(`{"t":"capdrv"}` + "\n")
	err := c.stream.Send(&pushpb.ClientMsg{Msg: &pushpb.ClientMsg_Append{Append: &pushpb.Append{
		Seq: 1, Harness: "claude-code", SrcId: wire.SrcID(path), Offset: 0, HeadHash: "capdrv",
		SourceName: "claude", Path: path, RawEnd: int64(len(body)), Body: body,
	}}})
	if err != nil {
		c.code = status.Code(err)
		return
	}
	msg, err := c.stream.Recv()
	if err != nil {
		c.code = status.Code(err)
		return
	}
	c.acked = msg.GetAck() != nil
}

func (c *client) watch() {
	go func() {
		_, err := c.stream.Recv()
		c.done <- status.Code(err)
	}()
}

func (c *client) close() {
	c.cancel()
	c.conn.Close()
}

func ack(c *client) string {
	if c.acked {
		return "ack"
	}
	return "-"
}

type silent struct {
	conn   *grpc.ClientConn
	cancel context.CancelFunc
	start  time.Time
	code   codes.Code
	msg    string
	secs   float64
	done   chan struct{}
}

func openSilent(server string, dial grpc.DialOption) (*silent, error) {
	conn, err := grpc.NewClient("passthrough:///"+server, grpc.WithTransportCredentials(insecure.NewCredentials()), dial)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := pushpb.NewPushClient(conn).Sync(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	s := &silent{conn: conn, cancel: cancel, start: time.Now(), done: make(chan struct{})}
	go func() {
		_, err := stream.Recv()
		s.secs = time.Since(s.start).Seconds()
		s.code = status.Code(err)
		s.msg = status.Convert(err).Message()
		close(s.done)
	}()
	return s, nil
}

func runSilent(server string, dial grpc.DialOption, capN int, prefix string) bool {
	held := capN + 4
	ss := make([]*silent, 0, held+1)
	for i := 0; i <= held; i++ {
		s, err := openSilent(server, dial)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return false
		}
		ss = append(ss, s)
		time.Sleep(200 * time.Millisecond)
	}
	extra := ss[held]
	select {
	case <-extra.done:
	case <-time.After(10 * time.Second):
	}
	fmt.Printf("silent extra stream %d code=%s secs=%.1f msg=%q\n", held+1, extra.code, extra.secs, extra.msg)
	ok := extra.code == codes.ResourceExhausted && extra.secs < 5
	dropped := 0
	for i, s := range ss[:held] {
		select {
		case <-s.done:
		case <-time.After(90 * time.Second):
		}
		fmt.Printf("silent stream %d code=%s secs=%.1f msg=%q\n", i+1, s.code, s.secs, s.msg)
		if s.code == codes.DeadlineExceeded && strings.Contains(s.msg, "no Hello within") && s.secs >= 55 && s.secs <= 70 {
			dropped++
		}
	}
	c, err := open(server, dial, prefix+"-after")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return false
	}
	c.welcome()
	c.appendAck()
	fmt.Printf("silent reuse %s %s %s\n", c.machine, c.code, ack(c))
	c.close()
	for _, s := range ss {
		s.cancel()
		s.conn.Close()
	}
	ok = ok && dropped == held && c.code == codes.OK && c.acked
	fmt.Printf("silent summary held=%d dropped_at_60s=%d reuse_acked=%v pass=%v\n", held, dropped, c.acked, ok)
	return ok
}

func main() {
	server := flag.String("server", "", "host:port of the QUIC listener")
	serverKey := flag.String("server-key", "", "the server's key")
	keyFile := flag.String("key-file", "", "client key file, created if missing")
	capN := flag.Int("cap", 10, "the server's live-session cap")
	prefix := flag.String("prefix", "cap", "machine name prefix")
	quiet := flag.Bool("silent", false, "run the silent pre-Hello stream gate instead of the session cap gate")
	flag.Parse()
	priv, err := qnet.LoadOrCreateKey(*keyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	dial := grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
		return qnet.Dial(ctx, addr, priv, *serverKey, qnet.ALPNPush)
	})
	if *quiet {
		if !runSilent(*server, dial, *capN, *prefix) {
			os.Exit(1)
		}
		return
	}
	n := *capN + 1
	cs := make([]*client, n)
	var wg sync.WaitGroup
	for i := range cs {
		c, err := open(*server, dial, fmt.Sprintf("%s%02d", *prefix, i))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		cs[i] = c
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.welcome()
			c.appendAck()
		}()
		time.Sleep(200 * time.Millisecond)
	}
	wg.Wait()
	exhausted, acked := 0, 0
	for i, c := range cs {
		fmt.Printf("client %d %s %s %s\n", i, c.machine, c.code, ack(c))
		if c.code == codes.ResourceExhausted {
			exhausted++
		}
		if c.acked {
			acked++
			c.watch()
		}
	}
	var first *client
	for _, c := range cs {
		if c.acked {
			first = c
			break
		}
	}
	ok := exhausted == 1 && acked == n-1
	if first != nil {
		t, err := open(*server, dial, first.machine)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		t.welcome()
		t.appendAck()
		var old codes.Code
		select {
		case old = <-first.done:
		case <-time.After(10 * time.Second):
			old = codes.DeadlineExceeded
		}
		fmt.Printf("takeover %s %s %s old=%s\n", t.machine, t.code, ack(t), old)
		ok = ok && t.code == codes.OK && old == codes.Aborted
		t.close()
	}
	for _, c := range cs {
		c.close()
	}
	fmt.Printf("summary cap=%d clients=%d acked=%d resource_exhausted=%d pass=%v\n", *capN, n, acked, exhausted, ok)
	if !ok {
		os.Exit(1)
	}
}
