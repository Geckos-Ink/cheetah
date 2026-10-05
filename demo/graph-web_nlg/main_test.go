package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeDataset(t *testing.T, dir, split string, rows []example) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"language": "en", "values": rows})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, split+".json"), b, 0600); err != nil {
		t.Fatal(err)
	}
}
func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var train []example
	for i := 0; i < 36; i++ {
		s := fmt.Sprintf("Entity_%02d", i)
		train = append(train, example{ID: fmt.Sprintf("train/%d", i), Input: []string{s + " | locatedIn | Hub_City", s + " | name | \"München, café '+:/\""}, Target: []string{fmt.Sprintf("Entity %02d is in Hub City, and its name is München café.", i)}, Category: "City"})
	}
	train = append(train, example{ID: "train/chain", Input: []string{"Hub_City | country | Country", "Country | continent | Europe"}, Target: []string{"Hub City is in Country, which is in Europe."}, Category: "City"})
	eval := []example{{ID: "validation/known", Input: []string{"Entity_00 | locatedIn | Hub_City"}, Target: []string{"Entity 00 is located in Hub City."}, Category: "City"}, {ID: "validation/unseen", Input: []string{"Unseen | country | Elsewhere"}, Target: []string{"Unseen is in Elsewhere."}, Category: "City"}}
	writeDataset(t, dir, "train", train)
	writeDataset(t, dir, "validation", eval)
	return dir
}
func TestLoadAndIdentity(t *testing.T) {
	dir := fixture(t)
	a, total, err := loadExamples(filepath.Join(dir, "train.json"), 10, 42)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := loadExamples(filepath.Join(dir, "train.json"), 10, 42)
	if err != nil {
		t.Fatal(err)
	}
	if total != 37 || len(a) != 10 || !reflect.DeepEqual(a, b) {
		t.Fatalf("unstable sample: total=%d n=%d", total, len(a))
	}
	all, _, err := loadExamples(filepath.Join(dir, "train.json"), 0, 42)
	if err != nil {
		t.Fatal(err)
	}
	c := buildCorpus(all)
	if len(c.Edges) != 74 || c.ReferencesDropped == 0 {
		t.Fatalf("unexpected corpus: edges=%d dropped=%d", len(c.Edges), c.ReferencesDropped)
	}
	for fact := range c.Facts {
		back, err := edgeTriple(tripleEdge(fact))
		if err != nil || back != fact {
			t.Fatalf("identity lost: %+v %+v %v", fact, back, err)
		}
	}
	for _, n := range c.Nodes {
		if len(n.References) > 9 {
			t.Fatal("reference cap")
		}
	}
	if token("e", "A_B") == token("e", "A B") || token("e", "x") == token("e", "\"x\"") {
		t.Fatal("merged distinct values")
	}
	twice := buildCorpus(append(all, all...))
	if !reflect.DeepEqual(c, twice) {
		t.Fatal("duplicate entries changed corpus")
	}
}
func TestRejectMalformedDatasets(t *testing.T) {
	for _, raw := range []string{`{}`, `{"language":"en","values":[]}`, `{"language":"en","values":[{"webnlg-id":"x","input":["a | b"],"target":["text"]}]}`, `{"language":"en","values":[]} {}`, `{"language":"en","values":[{"input":["a | b | c"],"target":["text"]}]}`} {
		p := filepath.Join(t.TempDir(), "bad.json")
		os.WriteFile(p, []byte(raw), 0600)
		if _, _, err := loadExamples(p, 1, 1); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, s := range []string{"a|b|c", "a |  | c", "a | b | c | d"} {
		if _, err := parseTriple(s); err == nil {
			t.Fatalf("accepted %s", s)
		}
	}
}
func TestExtractionScores(t *testing.T) {
	a := triple{"a", "p", "b"}
	b := triple{"b", "p", "c"}
	cases := []evaluationCase{{ID: "one", Gold: []triple{a}}, {ID: "two", Gold: []triple{b}}}
	p := filepath.Join(t.TempDir(), "predictions.jsonl")
	data := `{"id":"one","triples":[{"subject":"a","predicate":"p","object":"b"},{"subject":"a","predicate":"p","object":"b"}]}`
	os.WriteFile(p, []byte(data), 0600)
	m, err := scorePredictions(p, cases)
	if err != nil {
		t.Fatal(err)
	}
	if m.Precision != 1 || m.Recall != .5 || m.ExactMatch != .5 || m.Submitted != 1 {
		t.Fatalf("bad metrics: %+v", m)
	}
	os.WriteFile(p, []byte(data+"\n"+data), 0600)
	if _, err := scorePredictions(p, cases); err == nil {
		t.Fatal("duplicate predictions accepted")
	}
}
func TestConfigurationBounds(t *testing.T) {
	for _, mutate := range []func(*config){func(c *config) { c.Workers = 0 }, func(c *config) { c.Batch = 0 }, func(c *config) { c.Database = "../db" }, func(c *config) { c.EvalSplit = "train" }, func(c *config) { c.Reset = true; c.VerifyOnly = true }} {
		c := defaults()
		mutate(&c)
		if c.check() == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
}

func TestWebNLGEndToEnd(t *testing.T) {
	if os.Getenv("CHEETAH_WEBNLG_E2E") != "1" {
		t.Skip("set CHEETAH_WEBNLG_E2E=1 to build and boot an isolated server")
	}
	bin := filepath.Join(t.TempDir(), "server")
	if out, err := exec.Command("go", "build", "-o", bin, "cheetahdb/src").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	for _, stride := range []int{1, 2} {
		t.Run(fmt.Sprintf("pair_bytes_%d", stride), func(t *testing.T) {
			dir := t.TempDir()
			addr := freeAddress(t)
			stop := startServer(t, bin, dir, addr)
			cfg := defaults()
			cfg.Addr = addr
			cfg.Dataset = fixture(t)
			cfg.MaxTrain = 0
			cfg.MaxEval = 0
			cfg.Queries = 50
			cfg.Rounds = 3
			cfg.PairBytes = stride
			cfg.Reset = true
			cfg.ReportDir = t.TempDir()
			r, err := run(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !r.Validated || r.Edges != 74 || r.Retrieval.KnownTriples != 1 || r.Retrieval.GoldTriples != 2 || r.Retrieval.RecoveredKnown != 1 || r.StressEdgeWrites != 222 || r.QueryLatency.Count != 50 {
				t.Fatalf("bad report: %+v", r)
			}
			testMultiHopAndDelete(t, cfg)
			stop()
			stop = startServer(t, bin, dir, addr)
			defer stop()
			cfg.Reset = false
			cfg.VerifyOnly = true
			reopened, err := run(cfg)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			if !reopened.Validated || reopened.StressEdgeWrites != 0 {
				t.Fatalf("bad reopen report: %+v", reopened)
			}
			t.Logf("stride=%d nodes=%d edges=%d ingest=%.1f edges/s typed_query_p95=%.2fms known_recall=%.3f", stride, r.Nodes, r.Edges, r.IngestEdgesPerSecond, r.QueryLatency.P95, r.Retrieval.KnownRecall)
		})
	}
	if os.Getenv("CHEETAH_WEBNLG_REAL") == "1" {
		t.Run("real_dataset", func(t *testing.T) {
			cfg := defaults()
			cfg.Dataset = filepath.Join("..", "..", "studies", "datasets", "web_nlg")
			cfg.Addr = freeAddress(t)
			cfg.Reset = true
			cfg.ReportDir = t.TempDir()
			if s := os.Getenv("CHEETAH_WEBNLG_TRAIN"); s != "" {
				n, err := strconv.Atoi(s)
				if err != nil {
					t.Fatal(err)
				}
				cfg.MaxTrain = n
			}
			stop := startServer(t, bin, t.TempDir(), cfg.Addr)
			defer stop()
			r, err := run(cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("real WebNLG: train=%d nodes=%d edges=%d ingest=%.1f edges/s query_p95=%.2fms recall_p95=%.2fms coverage=%.3f known_recall=%.3f", r.TrainExamples, r.Nodes, r.Edges, r.IngestEdgesPerSecond, r.QueryLatency.P95, r.RecallLatency.P95, r.Retrieval.Coverage, r.Retrieval.KnownRecall)
		})
	}
}
func testMultiHopAndDelete(t *testing.T, cfg config) {
	t.Helper()
	cl, err := connect(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.conn.Close()
	got, err := queryEdges(cl, fmt.Sprintf("GRAPH_QUERY MATCH (id='%s')-[:*]->(id='%s') HOPS 2 BRANCH_LIMIT 128 COST_LIMIT 10", token("e", "Entity_00"), token("e", "Country")))
	if err != nil || len(got) == 0 {
		t.Fatalf("two-hop query: %+v %v", got, err)
	}
	e := tripleEdge(triple{"Entity_00", "locatedIn", "Hub_City"})
	args := fmt.Sprintf(" from=%s to=%s type=%s directed=1", e.From, e.To, e.Type)
	if _, err := cl.exec("GRAPH_EDGE_DEL" + args); err != nil {
		t.Fatal(err)
	}
	resp, err := cl.exec("GRAPH_EDGE_GET" + args)
	if err == nil || !strings.Contains(resp, "edge_not_found") {
		t.Fatalf("deleted edge still present: %s %v", resp, err)
	}
	for _, q := range []string{fmt.Sprintf("GRAPH_QUERY MATCH (id='%s')-[:%s]->(id='%s')", e.From, e.Type, e.To), fmt.Sprintf("GRAPH_QUERY MATCH (id='%s')<-[:%s]-(id='%s')", e.To, e.Type, e.From)} {
		got, err := queryEdges(cl, q)
		if err != nil || len(got) != 0 {
			t.Fatalf("stale deleted adjacency: %+v %v", got, err)
		}
	}
	if _, err := cl.exec("GRAPH_EDGE_SET_BATCH items=" + encoded([]edge{e})); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.exec("FILE_CHECKPOINT"); err != nil {
		t.Fatal(err)
	}
}
func freeAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}
func startServer(t *testing.T, bin, dir, addr string) func() {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "server.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CHEETAH_HEADLESS=1", "CHEETAH_LISTEN_ADDR="+addr, "CHEETAH_DATA_DIR="+dir, "CHEETAH_LOG_LEVEL=1", "CHEETAH_GRAPH_TERM_INDEX=1")
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			cmd.Process.Kill()
			<-done
			t.Error("server failed graceful shutdown")
		}
		log.Close()
		if t.Failed() {
			b, _ := os.ReadFile(logPath)
			t.Log(string(b))
		}
	}
	t.Cleanup(stop)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			c.Close()
			return stop
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	t.Fatal("server did not start")
	return stop
}
