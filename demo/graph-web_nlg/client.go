package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

type client struct {
	conn    net.Conn
	reader  *bufio.Reader
	timeout time.Duration
}

func connect(cfg config) (*client, error) {
	conn, err := net.DialTimeout("tcp", cfg.Addr, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	c := &client{conn, bufio.NewReader(conn), cfg.Timeout}
	if _, err = c.exec("DATABASE " + cfg.Database); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}
func (c *client) exec(cmd string) (string, error) {
	if strings.ContainsAny(cmd, "\r\n") {
		return "", fmt.Errorf("multiline command refused")
	}
	if err := c.conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return "", err
	}
	if _, err := io.WriteString(c.conn, cmd+"\n"); err != nil {
		return "", err
	}
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "SUCCESS") {
		return line, fmt.Errorf("%s: %s", strings.Fields(cmd)[0], line)
	}
	return line, nil
}
func field(s, key string) string {
	for _, p := range strings.Split(s, ",") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(p), key+"="); ok {
			return v
		}
	}
	return ""
}
func payload(s string, out any) error {
	b, err := base64.StdEncoding.DecodeString(field(s, "payload"))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
func encoded(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// One socket per worker, no shared reader and bounded work in flight. Stop each
// worker on failure; retain the first error and join every worker before return.
func parallel(cfg config, count int, work func(*client, int) error) error {
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	stop := make(chan struct{})
	fail := func(err error) { once.Do(func() { first = err; close(stop) }) }
	jobs := make(chan int)
	for w := 0; w < cfg.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := connect(cfg)
			if err != nil {
				fail(err)
				return
			}
			defer c.conn.Close()
			for {
				select {
				case <-stop:
					return
				case i, ok := <-jobs:
					if !ok {
						return
					}
					if err := work(c, i); err != nil {
						fail(err)
						return
					}
				}
			}
		}()
	}
feed:
	for i := 0; i < count; i++ {
		select {
		case <-stop:
			break feed
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
	return first
}
func ingest(cfg config, c corpus) error {
	if err := parallel(cfg, len(c.Nodes), func(cl *client, i int) error {
		n := c.Nodes[i]
		_, err := cl.exec("GRAPH_NODE_SET id=" + n.ID + " props=" + encoded(n.Props) + " references=" + encoded(n.References))
		return err
	}); err != nil {
		return err
	}
	return writeEdges(cfg, c.Edges, 1)
}
func writeEdges(cfg config, edges []edge, rounds int) error {
	batches := (len(edges) + cfg.Batch - 1) / cfg.Batch
	return parallel(cfg, batches*rounds, func(c *client, i int) error {
		start := (i % batches) * cfg.Batch
		end := min(start+cfg.Batch, len(edges))
		resp, err := c.exec("GRAPH_EDGE_SET_BATCH autocreate=0 items=" + encoded(edges[start:end]))
		if err != nil {
			return err
		}
		if field(resp, "applied") != fmt.Sprint(end-start) || field(resp, "failed") != "0" {
			return fmt.Errorf("incomplete batch: %s", resp)
		}
		return nil
	})
}
func queryEdges(c *client, base string) ([]edge, error) {
	var result []edge
	cursor := ""
	seen := map[string]bool{}
	for {
		cmd := base + " RETURN edges LIMIT 7"
		if cursor != "" {
			cmd += " CURSOR " + cursor
		}
		resp, err := c.exec(cmd)
		if err != nil {
			return nil, err
		}
		var page []edge
		if err := payload(resp, &page); err != nil {
			return nil, err
		}
		result = append(result, page...)
		cursor = field(resp, "next_cursor")
		if cursor == "*" || cursor == "-" || cursor == "" {
			return result, nil
		}
		if seen[cursor] {
			return nil, fmt.Errorf("query cursor repeated")
		}
		seen[cursor] = true
	}
}
func edgeSet(es []edge) map[edge]bool {
	m := map[edge]bool{}
	for _, e := range es {
		m[e] = true
	}
	return m
}
func sameEdges(got, want []edge) bool {
	if len(got) != len(want) {
		return false
	}
	m := edgeSet(want)
	seen := map[edge]bool{}
	for _, e := range got {
		if !m[e] || seen[e] {
			return false
		}
		seen[e] = true
	}
	return true
}

// Validate against an independent in-memory oracle, not response success alone.
func validate(cfg config, cor corpus) error {
	if err := parallel(cfg, len(cor.Nodes), func(c *client, i int) error {
		want := cor.Nodes[i]
		resp, err := c.exec("GRAPH_NODE_GET id=" + want.ID)
		if err != nil {
			return err
		}
		var got node
		if err := payload(resp, &got); err != nil {
			return err
		}
		if encoded(got) != encoded(want) {
			return fmt.Errorf("node/reference mismatch: %s", want.ID)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := parallel(cfg, len(cor.Edges), func(c *client, i int) error {
		want := cor.Edges[i]
		resp, err := c.exec(fmt.Sprintf("GRAPH_EDGE_GET from=%s to=%s type=%s directed=1", want.From, want.To, want.Type))
		if err != nil {
			return err
		}
		var got edge
		if err := payload(resp, &got); err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("edge mismatch: %+v != %+v", got, want)
		}
		return nil
	}); err != nil {
		return err
	}
	out, in := map[string][]edge{}, map[string][]edge{}
	for _, e := range cor.Edges {
		out[e.From] = append(out[e.From], e)
		in[e.To] = append(in[e.To], e)
	}
	return parallel(cfg, len(cor.Nodes), func(c *client, i int) error {
		id := cor.Nodes[i].ID
		for _, reverse := range []bool{false, true} {
			pattern := "-[:*]->(*)"
			want := out[id]
			if reverse {
				pattern = "<-[:*]-(*)"
				want = in[id]
			}
			got, err := queryEdges(c, "GRAPH_QUERY MATCH (id='"+id+"')"+pattern)
			if err != nil {
				return err
			}
			if !sameEdges(got, want) {
				return fmt.Errorf("adjacency mismatch id=%s reverse=%t got=%d want=%d", id, reverse, len(got), len(want))
			}
			if len(want) > 7 {
				// Exclude the first scanned endpoint so filtering crosses an
				// internal page boundary before filling the external page.
				scope, excluded := "to", got[0].To
				if reverse {
					scope, excluded = "from", got[0].From
				}
				var filtered []edge
				for _, e := range want {
					endpoint := e.To
					if reverse {
						endpoint = e.From
					}
					if endpoint != excluded {
						filtered = append(filtered, e)
					}
				}
				rows, err := queryEdges(c, "GRAPH_QUERY MATCH (id='"+id+"')"+pattern+" WHERE "+scope+".id != '"+excluded+"'")
				if err != nil {
					return err
				}
				if !sameEdges(rows, filtered) {
					return fmt.Errorf("filtered adjacency mismatch id=%s reverse=%t got=%d want=%d", id, reverse, len(rows), len(filtered))
				}
			}
		}
		return nil
	})
}
