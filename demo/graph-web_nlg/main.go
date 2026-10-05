// graph-web_nlg is a TCP client, not part of the server binary.
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type config struct {
	Addr        string        `json:"address"`
	Database    string        `json:"database"`
	Dataset     string        `json:"dataset"`
	EvalSplit   string        `json:"eval_split"`
	MaxTrain    int           `json:"max_train"`
	MaxEval     int           `json:"max_eval"`
	Seed        int64         `json:"seed"`
	Workers     int           `json:"workers"`
	Batch       int           `json:"batch"`
	Rounds      int           `json:"stress_rounds"`
	Queries     int           `json:"queries"`
	PairBytes   int           `json:"pair_bytes"`
	Reset       bool          `json:"reset"`
	VerifyOnly  bool          `json:"verify_only"`
	Timeout     time.Duration `json:"timeout_ns"`
	ReportDir   string        `json:"report_dir"`
	Predictions string        `json:"predictions,omitempty"`
}
type latency struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50_ms"`
	P95   float64 `json:"p95_ms"`
	P99   float64 `json:"p99_ms"`
}
type retrievalMetrics struct {
	AdjacencyTruncated int     `json:"seed_adjacency_truncated_pages"`
	Examples           int     `json:"examples"`
	GoldTriples        int     `json:"gold_triples"`
	KnownTriples       int     `json:"known_training_triples"`
	RecoveredKnown     int     `json:"recovered_known_triples"`
	RetrievedTriples   int     `json:"retrieved_triples"`
	CorrectTriples     int     `json:"correct_triples"`
	Coverage           float64 `json:"training_coverage"`
	KnownRecall        float64 `json:"known_triple_recall"`
	Recall             float64 `json:"all_gold_recall"`
	Precision          float64 `json:"evidence_precision"`
	Truncated          int     `json:"truncated_queries"`
}
type report struct {
	Config               config             `json:"config"`
	TrainRows            int                `json:"train_rows_total"`
	EvalRows             int                `json:"eval_rows_total"`
	TrainExamples        int                `json:"train_examples_loaded"`
	Nodes                int                `json:"unique_nodes"`
	Edges                int                `json:"unique_edges"`
	ReferencesDropped    int                `json:"references_omitted_by_cap"`
	IngestSeconds        float64            `json:"ingest_seconds"`
	IngestEdgesPerSecond float64            `json:"ingest_edges_per_second"`
	StressSeconds        float64            `json:"stress_seconds"`
	StressEdgeWrites     int                `json:"stress_edge_writes"`
	QueryLatency         latency            `json:"exact_query_latency"`
	RecallLatency        latency            `json:"text_retrieval_latency"`
	Retrieval            retrievalMetrics   `json:"heldout_text_retrieval"`
	Extraction           *extractionMetrics `json:"llm_extraction,omitempty"`
	Validated            bool               `json:"validated"`
	Stats                string             `json:"system_stats"`
}
type association struct {
	ID         string      `json:"id"`
	Via        []edge      `json:"via"`
	References []reference `json:"references"`
}
type recallResult struct {
	Seeds []struct {
		Matches []struct {
			ID string `json:"id"`
		} `json:"matches"`
	} `json:"seeds"`
	Associations []association `json:"associations"`
}
type evaluationCase struct {
	ID               string      `json:"id"`
	Category         string      `json:"category"`
	Text             string      `json:"text"`
	Gold             []triple    `json:"gold_triples"`
	Evidence         []triple    `json:"retrieved_triples"`
	References       []reference `json:"training_references"`
	ExtractionPrompt string      `json:"extraction_prompt"`
	GenerationPrompt string      `json:"generation_prompt"`
}

func defaults() config {
	return config{Addr: "127.0.0.1:4455", Database: "graph_web_nlg_demo", Dataset: "studies/datasets/web_nlg", EvalSplit: "validation", MaxTrain: 300, MaxEval: 100, Seed: 42, Workers: 4, Batch: 32, Rounds: 2, Queries: 100, PairBytes: 1, Timeout: 30 * time.Second, ReportDir: "demo/graph-web_nlg/reports"}
}
func main() {
	c := defaults()
	flag.StringVar(&c.Addr, "addr", c.Addr, "Cheetah TCP address")
	flag.StringVar(&c.Database, "database", c.Database, "logical database dedicated to this demo")
	flag.StringVar(&c.Dataset, "dataset", c.Dataset, "directory containing English train/validation/test JSON exports")
	flag.StringVar(&c.EvalSplit, "eval-split", c.EvalSplit, "validation or test; never ingested")
	flag.IntVar(&c.MaxTrain, "max-train", c.MaxTrain, "sampled training entries (0 = full split)")
	flag.IntVar(&c.MaxEval, "max-eval", c.MaxEval, "sampled evaluation entries (0 = full split)")
	flag.Int64Var(&c.Seed, "seed", c.Seed, "deterministic reservoir sampling seed")
	flag.IntVar(&c.Workers, "workers", c.Workers, "concurrent TCP clients (1..64)")
	flag.IntVar(&c.Batch, "batch-size", c.Batch, "edges per batch (1..256)")
	flag.IntVar(&c.Rounds, "stress-rounds", c.Rounds, "concurrent idempotent edge rewrite rounds")
	flag.IntVar(&c.Queries, "queries", c.Queries, "exact typed query checks during stress")
	flag.IntVar(&c.PairBytes, "pair-bytes", c.PairBytes, "trie stride on reset: 1 or 2")
	flag.BoolVar(&c.Reset, "reset-db", false, "explicitly reset only the named demo database")
	flag.BoolVar(&c.VerifyOnly, "verify-only", false, "validate existing data without ingest or stress writes (for reopen checks)")
	flag.DurationVar(&c.Timeout, "timeout", c.Timeout, "per-command network deadline")
	flag.StringVar(&c.ReportDir, "report-dir", c.ReportDir, "JSON report and JSONL LLM cases directory; empty disables artifacts")
	flag.StringVar(&c.Predictions, "predictions", "", "optional JSONL LLM extraction output: {id,triples:[{subject,predicate,object}]}")
	flag.Parse()
	r, err := run(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println(string(b))
}
func (c config) check() error {
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(c.Database) {
		return fmt.Errorf("database must contain only letters, numbers, '_' or '-'")
	}
	if c.Workers < 1 || c.Workers > 64 || c.Batch < 1 || c.Batch > 256 || c.MaxTrain < 0 || c.MaxEval < 0 || c.Rounds < 0 || c.Queries < 0 || c.Timeout <= 0 {
		return fmt.Errorf("invalid workload bounds")
	}
	if c.PairBytes != 1 && c.PairBytes != 2 {
		return fmt.Errorf("pair-bytes must be 1 or 2")
	}
	if c.EvalSplit != "validation" && c.EvalSplit != "test" {
		return fmt.Errorf("eval-split must be validation or test")
	}
	if c.Reset && c.VerifyOnly {
		return fmt.Errorf("reset-db and verify-only conflict")
	}
	return nil
}
func run(cfg config) (report, error) {
	r := report{Config: cfg}
	if err := cfg.check(); err != nil {
		return r, err
	}
	train, n, err := loadExamples(filepath.Join(cfg.Dataset, "train.json"), cfg.MaxTrain, cfg.Seed)
	if err != nil {
		return r, err
	}
	r.TrainRows = n
	r.TrainExamples = len(train)
	eval, n, err := loadExamples(filepath.Join(cfg.Dataset, cfg.EvalSplit+".json"), cfg.MaxEval, cfg.Seed+1)
	if err != nil {
		return r, err
	}
	r.EvalRows = n
	trainIDs := map[string]bool{}
	for _, e := range train {
		trainIDs[e.ID] = true
	}
	for _, e := range eval {
		if trainIDs[e.ID] {
			return r, fmt.Errorf("entry id %s crosses splits", e.ID)
		}
	}
	cor := buildCorpus(train)
	r.Nodes = len(cor.Nodes)
	r.Edges = len(cor.Edges)
	r.ReferencesDropped = cor.ReferencesDropped
	fmt.Fprintf(os.Stderr, "WebNLG: %d/%d training entries, %d nodes, %d unique triples; %d held-out %s entries\n", len(train), r.TrainRows, r.Nodes, r.Edges, len(eval), cfg.EvalSplit)
	cl, err := connect(cfg)
	if err != nil {
		return r, err
	}
	defer cl.conn.Close()
	if cfg.Reset {
		if _, err := cl.exec(fmt.Sprintf("RESET_DB %s pair_bytes=%d", cfg.Database, cfg.PairBytes)); err != nil {
			return r, err
		}
	}
	if !cfg.VerifyOnly {
		start := time.Now()
		if err := ingest(cfg, cor); err != nil {
			return r, fmt.Errorf("ingest: %w", err)
		}
		r.IngestSeconds = time.Since(start).Seconds()
		r.IngestEdgesPerSecond = float64(r.Edges) / r.IngestSeconds
	}
	fmt.Fprintln(os.Stderr, "Validating every node, reference, edge and paged forward/reverse adjacency...")
	if err := validate(cfg, cor); err != nil {
		return r, err
	}
	start := time.Now()
	var samples []time.Duration
	// Readers race with idempotent writes to the same edges; final oracle checks
	// catch corruption hidden by a successful acknowledgement.
	writeDone := make(chan error, 1)
	if !cfg.VerifyOnly {
		go func() { writeDone <- writeEdges(cfg, cor.Edges, cfg.Rounds) }()
	} else {
		writeDone <- nil
	}
	samples, err = benchmark(cfg, cor.Edges)
	writeErr := <-writeDone
	if err != nil {
		return r, err
	}
	if writeErr != nil {
		return r, writeErr
	}
	r.StressSeconds = time.Since(start).Seconds()
	if !cfg.VerifyOnly {
		r.StressEdgeWrites = len(cor.Edges) * cfg.Rounds
	}
	r.QueryLatency = percentiles(samples)
	if _, err := cl.exec("FILE_CHECKPOINT"); err != nil {
		return r, err
	}
	if err := validate(cfg, cor); err != nil {
		return r, fmt.Errorf("post-stress: %w", err)
	}
	fmt.Fprintln(os.Stderr, "Evaluating held-out sentence retrieval and exporting LLM cases...")
	cases, metrics, durations, err := evaluate(cfg, eval, cor)
	if err != nil {
		return r, err
	}
	r.Retrieval = metrics
	r.RecallLatency = percentiles(durations)
	if cfg.Predictions != "" {
		m, err := scorePredictions(cfg.Predictions, cases)
		if err != nil {
			return r, err
		}
		r.Extraction = &m
	}
	if r.Stats, err = cl.exec("SYSTEM_STATS"); err != nil {
		return r, err
	}
	r.Validated = true
	if cfg.ReportDir != "" {
		if err := writeArtifacts(cfg.ReportDir, r, cases); err != nil {
			return r, err
		}
	}
	return r, nil
}
func benchmark(cfg config, edges []edge) ([]time.Duration, error) {
	samples := make([]time.Duration, cfg.Queries)
	err := parallel(cfg, cfg.Queries, func(c *client, i int) error {
		e := edges[(i*7919)%len(edges)]
		cmd := fmt.Sprintf("GRAPH_QUERY MATCH (id='%s')-[:%s]->(id='%s')", e.From, e.Type, e.To)
		start := time.Now()
		got, err := queryEdges(c, cmd)
		samples[i] = time.Since(start)
		if err != nil {
			return err
		}
		if !sameEdges(got, []edge{e}) {
			return fmt.Errorf("typed query mismatch for %+v", e)
		}
		return nil
	})
	return samples, err
}
func evaluate(cfg config, examples []example, cor corpus) ([]evaluationCase, retrievalMetrics, []time.Duration, error) {
	cases := make([]evaluationCase, len(examples))
	durations := make([]time.Duration, len(examples))
	truncated := make([]bool, len(examples))
	adjacencyTruncated := make([]int, len(examples))
	err := parallel(cfg, len(examples), func(c *client, i int) error {
		e := examples[i]
		text := e.Target[0]
		start := time.Now()
		// Sentences supply lexical seeds; gold subjects are deliberately not used.
		resp, err := c.exec("GRAPH_RECALL seeds=base64:" + base64.StdEncoding.EncodeToString([]byte(text)) + " expand=lexical hops=2 direction=both precision=0.01 limit=64 seed_limit=16 branch_limit=128 budget=2048 cache=off include_seeds=1 references=1 reference_limit=32")
		if err != nil {
			return err
		}
		truncated[i] = field(resp, "truncated") == "1"
		var result recallResult
		if err := payload(resp, &result); err != nil {
			return err
		}
		evidence := map[triple]bool{}
		refs := map[string]reference{}
		for _, a := range result.Associations {
			for _, v := range a.Via {
				t, err := edgeTriple(v)
				if err != nil {
					return err
				}
				if !cor.Facts[t] {
					return fmt.Errorf("recall returned non-training edge: %+v", t)
				}
				evidence[t] = true
			}
			for _, ref := range a.References {
				if ref.Source != "webnlg:entity" {
					refs[ref.ID] = ref
				}
			}
		}
		// Seed nodes can already be endpoints of the desired relationship. Their
		// best recall trace is then length zero, so `via` alone omits that fact.
		// Hydrate a bounded neighbourhood of the top eight resolved seeds.
		seeds := map[string]bool{}
		for _, resolution := range result.Seeds {
			for _, match := range resolution.Matches {
				if seeds[match.ID] || len(seeds) >= 8 {
					continue
				}
				seeds[match.ID] = true
				resp, err := c.exec("GRAPH_NEIGHBORS id=" + match.ID + " direction=out limit=32")
				if err != nil {
					return err
				}
				if cursor := field(resp, "next_cursor"); cursor != "" && cursor != "*" {
					adjacencyTruncated[i]++
				}
				var neighbours []edge
				if err := payload(resp, &neighbours); err != nil {
					return err
				}
				for _, v := range neighbours {
					fact, err := edgeTriple(v)
					if err != nil {
						return err
					}
					if !cor.Facts[fact] {
						return fmt.Errorf("seed adjacency returned non-training edge: %+v", fact)
					}
					evidence[fact] = true
				}
			}
		}
		durations[i] = time.Since(start)
		ev := sortedTriples(evidence)
		gold := map[triple]bool{}
		for _, t := range e.Triples {
			gold[t] = true
		}
		row := evaluationCase{ID: e.ID, Category: e.Category, Text: text, Gold: sortedTriples(gold), Evidence: ev}
		for _, ref := range refs {
			row.References = append(row.References, ref)
		}
		sort.Slice(row.References, func(i, j int) bool { return row.References[i].ID < row.References[j].ID })
		row.ExtractionPrompt = "Extract directed RDF triples from the following text. Return JSON {id,triples:[{subject,predicate,object}]}. Preserve WebNLG entity spelling with underscores, literal quotes, and camelCase predicates. Do not invent facts. ID: " + e.ID + "\nText: " + text
		b, _ := json.Marshal(ev)
		row.GenerationPrompt = "Write a short description supported only by these retrieved triples. If none apply, say there is insufficient evidence. Do not invent relations.\nTriples: " + string(b)
		cases[i] = row
		return nil
	})
	m := retrievalMetrics{Examples: len(cases)}
	if err != nil {
		return nil, m, nil, err
	}
	for i, c := range cases {
		m.AdjacencyTruncated += adjacencyTruncated[i]
		if truncated[i] {
			m.Truncated++
		}
		retrieved := map[triple]bool{}
		for _, t := range c.Evidence {
			retrieved[t] = true
		}
		m.RetrievedTriples += len(retrieved)
		for _, t := range c.Gold {
			m.GoldTriples++
			if cor.Facts[t] {
				m.KnownTriples++
				if retrieved[t] {
					m.RecoveredKnown++
				}
			}
			if retrieved[t] {
				m.CorrectTriples++
			}
		}
	}
	m.Coverage = ratio(m.KnownTriples, m.GoldTriples)
	m.KnownRecall = ratio(m.RecoveredKnown, m.KnownTriples)
	m.Recall = ratio(m.CorrectTriples, m.GoldTriples)
	m.Precision = ratio(m.CorrectTriples, m.RetrievedTriples)
	return cases, m, durations, nil
}
func sortedTriples(m map[triple]bool) []triple {
	out := make([]triple, 0, len(m))
	for t := range m {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Subject != b.Subject {
			return a.Subject < b.Subject
		}
		if a.Predicate != b.Predicate {
			return a.Predicate < b.Predicate
		}
		return a.Object < b.Object
	})
	return out
}
func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}
func percentiles(ds []time.Duration) latency {
	if len(ds) == 0 {
		return latency{}
	}
	sorted := append([]time.Duration(nil), ds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p := func(q float64) float64 {
		return float64(sorted[int(math.Ceil(q*float64(len(sorted))))-1]) / float64(time.Millisecond)
	}
	return latency{len(ds), p(.5), p(.95), p(.99)}
}
func writeArtifacts(dir string, r report, cases []evaluationCase) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "web_nlg-*.json")
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	err = enc.Encode(r)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	path := strings.TrimSuffix(f.Name(), ".json") + "-cases.jsonl"
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	enc = json.NewEncoder(out)
	for _, c := range cases {
		if err := enc.Encode(c); err != nil {
			out.Close()
			return err
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Report: %s\nLLM cases: %s\n", f.Name(), path)
	return nil
}
