// fabric-peer is the guest workload of scripts/test-fabric.sh. Two pairs of
// endpoints each hold one long-lived TCP connection across checkpoints:
//
//   - server/client: request/response sequence numbers; the server checks for
//     gaps and answers with the contents of /id, so a fork shows which copy of
//     the server replied.
//   - bulk-server/bulk-client: a ~12 MB/s stream of counters, so socket queues
//     hold unacknowledged data when a checkpoint is taken.
//
// Status goes to files under /tmp. Clients retry their first dial, but
// nothing reconnects: a broken connection stays broken and visible. No deadlines either: they are timer-based and
// would fire spuriously after a restore.
package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

func writeStatus(path, s string) {
	tmp := path + ".tmp"
	_ = os.WriteFile(tmp, []byte(s+"\n"), 0o644)
	_ = os.Rename(tmp, path)
}

func readID() string {
	b, err := os.ReadFile("/id")
	if err != nil {
		return "unset"
	}
	return strings.TrimSpace(string(b))
}

func server(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Println("listen:", err)
		os.Exit(1)
	}
	var conns, gaps atomic.Int64
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		n := conns.Add(1)
		go func(c net.Conn, n int64) {
			defer c.Close()
			r := bufio.NewReader(c)
			last := int64(0)
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					writeStatus("/tmp/srv.status", fmt.Sprintf("conns=%d last=%d gaps=%d conn%d=closed:%v", conns.Load(), last, gaps.Load(), n, err))
					return
				}
				seq, _ := strconv.ParseInt(strings.TrimSpace(line), 10, 64)
				if seq != last+1 {
					gaps.Add(1)
					fmt.Printf("GAP conn%d expected %d got %d\n", n, last+1, seq)
				}
				last = seq
				if _, err := fmt.Fprintf(c, "%d %s\n", seq, readID()); err != nil {
					return
				}
				writeStatus("/tmp/srv.status", fmt.Sprintf("conns=%d last=%d gaps=%d", conns.Load(), last, gaps.Load()))
			}
		}(c, n)
	}
}

// dial retries for up to 10s, since a peer started at the same time may not
// be listening yet. Only the first connection is retried.
func dial(addr string) (net.Conn, error) {
	var err error
	for i := 0; i < 100; i++ {
		var c net.Conn
		if c, err = net.Dial("tcp", addr); err == nil {
			return c, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, err
}

func client(addr string) {
	c, err := dial(addr)
	if err != nil {
		writeStatus("/tmp/cli.status", "state=dial-failed:"+err.Error())
		select {}
	}
	r := bufio.NewReader(c)
	errors := 0
	for seq := int64(1); ; seq++ {
		state := "ok"
		if _, err := fmt.Fprintf(c, "%d\n", seq); err != nil {
			state = "dead:" + err.Error()
		}
		var reply string
		if state == "ok" {
			reply, err = r.ReadString('\n')
			if err != nil {
				state = "dead:" + err.Error()
			}
		}
		f := strings.Fields(reply)
		peer := ""
		if len(f) == 2 {
			peer = f[1]
			if f[0] != strconv.FormatInt(seq, 10) {
				errors++
			}
		}
		writeStatus("/tmp/cli.status", fmt.Sprintf("last=%d peer=%s errors=%d state=%s", seq, peer, errors, state))
		if state != "ok" {
			select {} // keep the process for inspection; never reconnect
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// bulkServer verifies a stream of big-endian uint64 counters.
func bulkServer(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Println("listen:", err)
		os.Exit(1)
	}
	conns := 0
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		conns++
		go func(c net.Conn, n int) {
			r := bufio.NewReaderSize(c, 1<<16)
			var want, gaps uint64
			buf := make([]byte, 8)
			for {
				if _, err := io.ReadFull(r, buf); err != nil {
					writeStatus("/tmp/bulk.status", fmt.Sprintf("conns=%d recv=%d gaps=%d state=closed:%v", n, want, gaps, err))
					return
				}
				if v := binary.BigEndian.Uint64(buf); v != want {
					gaps++
					want = v
				}
				want++
				if want%8192 == 0 {
					writeStatus("/tmp/bulk.status", fmt.Sprintf("conns=%d recv=%d gaps=%d state=ok", n, want, gaps))
				}
			}
		}(c, conns)
	}
}

// bulkClient writes counters at ~12 MB/s so socket queues are rarely empty.
func bulkClient(addr string) {
	c, err := dial(addr)
	if err != nil {
		writeStatus("/tmp/bulkc.status", "state=dial-failed:"+err.Error())
		select {}
	}
	buf := make([]byte, 1<<16)
	var next uint64
	for {
		for i := 0; i < len(buf); i += 8 {
			binary.BigEndian.PutUint64(buf[i:], next)
			next++
		}
		if _, err := c.Write(buf); err != nil {
			writeStatus("/tmp/bulkc.status", fmt.Sprintf("sent=%d state=dead:%v", next, err))
			select {}
		}
		writeStatus("/tmp/bulkc.status", fmt.Sprintf("sent=%d state=ok", next))
		time.Sleep(5 * time.Millisecond)
	}
}

func main() {
	if len(os.Args) != 3 {
		fmt.Println("usage: fabric-peer server|client ADDR")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "server":
		server(os.Args[2])
	case "client":
		client(os.Args[2])
	case "bulk-server":
		bulkServer(os.Args[2])
	case "bulk-client":
		bulkClient(os.Args[2])
	}
}
