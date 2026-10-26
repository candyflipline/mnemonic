// Command caddydrv is the box tool behind caddy.sh: a bare Push.Sync client
// that sends synthesized appends and prints what it saw. It dials the way the
// product client does (https system roots, http plaintext, keepalive 30 s /
// 20 s) and never disables verification; the box trusts Caddy's local CA
// through SSL_CERT_FILE. Tooling only.
//
//	caddydrv hold  -server URL -token T -machine M     append every 200 ms until killed; prints "ready" after the first ack
//	caddydrv lat   -server URL -token T -machine M -n N  N appends, one at a time; prints "lat_us <n>" per append, then "done"
//	caddydrv big   -server URL -token T -machine M -mib N -chunk-mib C  windowed appends totalling N MiB; prints "sent/acked/bytes"
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/url"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/candyflipline/mnemonic/internal/wire/pushpb"
)

type sess struct {
	stream   pushpb.Push_SyncClient
	conn     *grpc.ClientConn
	machine  string
	path     string
	srcID    string
	windowN  uint32
	windowB  uint64
	seq      uint64
	size     int64
	ackReady chan *pushpb.Ack
	recvErr  chan error
}

func dial(server, token, machine string) (*sess, error) {
	u, err := url.Parse(server)
	if err != nil {
		return nil, err
	}
	port := u.Port()
	var creds credentials.TransportCredentials
	switch u.Scheme {
	case "https":
		if port == "" {
			port = "443"
		}
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	case "http":
		if port == "" {
			port = "80"
		}
		creds = insecure.NewCredentials()
	default:
		return nil, fmt.Errorf("server %q is not an http(s) URL", server)
	}
	conn, err := grpc.NewClient("passthrough:///"+u.Hostname()+":"+port,
		grpc.WithTransportCredentials(creds),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 20 * time.Second}),
	)
	if err != nil {
		return nil, err
	}
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
	stream, err := pushpb.NewPushClient(conn).Sync(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&pushpb.ClientMsg{Msg: &pushpb.ClientMsg_Hello{Hello: &pushpb.Hello{Machine: machine}}}); err != nil {
		return nil, err
	}
	s := &sess{stream: stream, conn: conn, machine: machine}
	s.path = "/gcaddy/" + machine + ".jsonl"
	s.srcID = wire.SrcID(s.path)
	for {
		msg, err := stream.Recv()
		if err != nil {
			return nil, fmt.Errorf("welcome: %w", err)
		}
		w := msg.GetWelcome()
		if w == nil {
			return nil, fmt.Errorf("welcome: got an ack")
		}
		for _, st := range w.Sources {
			if st.SrcId == s.srcID {
				s.size = st.Size
			}
		}
		if w.More {
			continue
		}
		s.windowN, s.windowB = w.Window.Appends, w.Window.Bytes
		break
	}
	s.ackReady = make(chan *pushpb.Ack, 1024)
	s.recvErr = make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				s.recvErr <- err
				return
			}
			a := msg.GetAck()
			if a == nil {
				s.recvErr <- fmt.Errorf("server sent a non-ack")
				return
			}
			s.ackReady <- a
		}
	}()
	return s, nil
}

func (s *sess) send(body []byte) error {
	s.seq++
	err := s.stream.Send(&pushpb.ClientMsg{Msg: &pushpb.ClientMsg_Append{Append: &pushpb.Append{
		Seq: s.seq, Harness: "claude-code", SrcId: s.srcID, Offset: s.size, HeadHash: "gcaddy",
		SourceName: "claude", Path: s.path, RawEnd: s.size + int64(len(body)), Body: body,
	}}})
	if err == nil {
		s.size += int64(len(body))
	}
	return err
}

func (s *sess) wait(d time.Duration) (*pushpb.Ack, error) {
	select {
	case a := <-s.ackReady:
		return a, nil
	case err := <-s.recvErr:
		return nil, err
	case <-time.After(d):
		return nil, fmt.Errorf("no ack within %v", d)
	}
}

func line(n int, tag string) []byte {
	b := make([]byte, 0, n)
	b = append(b, fmt.Sprintf(`{"t":%q,"x":"`, tag)...)
	for len(b) < n-3 {
		b = append(b, "lorem ipsum dolor sit amet "...)
	}
	b = append(b[:n-3], `"}`...)
	return append(b, '\n')
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: caddydrv hold|lat|big -server URL -token T -machine M")
		os.Exit(2)
	}
	mode := os.Args[1]
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	server := fs.String("server", "", "https://host:port or http://host:port")
	token := fs.String("token", "", "bearer token")
	machine := fs.String("machine", "", "machine segment")
	n := fs.Int("n", 20, "lat: appends")
	bodyN := fs.Int("body", 4096, "lat/hold: bytes per append")
	mib := fs.Int("mib", 330, "big: total MiB")
	chunk := fs.Int("chunk-mib", 30, "big: MiB per append")
	ackWait := fs.Duration("ack-wait", 30*time.Second, "give up on one ack after")
	fs.Parse(os.Args[2:])
	s, err := dial(*server, *token, *machine)
	if err != nil {
		fmt.Println("error dial:", err)
		os.Exit(1)
	}
	defer s.conn.Close()
	switch mode {
	case "hold":
		first := true
		for {
			if err := s.send(line(*bodyN, "hold")); err != nil {
				fmt.Println("error send:", err)
				os.Exit(1)
			}
			if _, err := s.wait(*ackWait); err != nil {
				fmt.Println("error ack:", err)
				os.Exit(1)
			}
			if first {
				fmt.Println("ready")
				first = false
			}
			time.Sleep(200 * time.Millisecond)
		}
	case "lat":
		bad := 0
		for i := 0; i < *n; i++ {
			body := line(*bodyN, fmt.Sprint("lat", i))
			t0 := time.Now()
			if err := s.send(body); err != nil {
				fmt.Println("error send:", err)
				os.Exit(1)
			}
			a, err := s.wait(*ackWait)
			d := time.Since(t0)
			if err != nil {
				fmt.Println("error ack:", err)
				bad++
				break
			}
			if a.Result != pushpb.Ack_APPLIED {
				bad++
			}
			fmt.Printf("lat_us %d\n", d.Microseconds())
		}
		_ = s.stream.CloseSend()
		fmt.Printf("done errors=%d\n", bad)
		if bad > 0 {
			os.Exit(1)
		}
	case "big":
		chunkB := *chunk << 20
		body := bytes.Repeat(line(1<<20, "big"), *chunk)
		total := (*mib << 20) / chunkB
		sent, acked, applied := 0, 0, 0
		var inflightB int64
		t0 := time.Now()
		for acked < total {
			for sent < total && sent-acked < int(s.windowN) && uint64(inflightB)+uint64(chunkB) <= s.windowB {
				if err := s.send(body); err != nil {
					fmt.Printf("error send after sent=%d acked=%d: %v\n", sent, acked, err)
					os.Exit(1)
				}
				sent++
				inflightB += int64(chunkB)
			}
			a, err := s.wait(*ackWait)
			if err != nil {
				fmt.Printf("error ack after sent=%d acked=%d: %v\n", sent, acked, err)
				os.Exit(1)
			}
			acked++
			inflightB -= int64(chunkB)
			if a.Result == pushpb.Ack_APPLIED {
				applied++
			}
		}
		_ = s.stream.CloseSend()
		fmt.Printf("sent=%d acked=%d applied=%d bytes=%d wall_s=%.1f\n", sent, acked, applied, int64(sent)*int64(chunkB), time.Since(t0).Seconds())
	default:
		fmt.Fprintln(os.Stderr, "unknown mode", mode)
		os.Exit(2)
	}
}
