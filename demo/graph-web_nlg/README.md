# WebNLG graph and language-grounding demo

A dependency-free Go TCP client for the English exports in
[`studies/datasets/web_nlg`](../../studies/datasets/web_nlg). It replaces the NELL demo.
WebNLG pairs directed RDF triples with human-written descriptions, so it lets us test
both graph storage and the evidence that an LLM would receive.

## Run against a running server

From the repository root:

```sh
go run ./demo/graph-web_nlg --reset-db
# Or: demo/graph-web_nlg/run.sh --reset-db
```

The default workload samples 300 training entries across the entire split, evaluates
100 validation entries, and uses four sockets, 32 edges per batch, two rewrite rounds,
and 100 typed query checks. It uses `127.0.0.1:4455` and database `graph_web_nlg_demo`.
Reset is **opt-in** and affects the named database. Use a dedicated demo database.
`--timeout 30s` bounds each command. A malformed dataset, failed/partial batch, or graph
mismatch exits nonzero; it is not counted as a successful benchmark.

A larger stress run:

```sh
go run ./demo/graph-web_nlg --reset-db --database web_nlg_stress \
  --max-train 0 --max-eval 0 --workers 8 --batch-size 128 \
  --stress-rounds 5 --queries 5000 --eval-split test --pair-bytes 2
```

`0` means the entire split. The loader uses deterministic reservoir sampling
(`--seed 42`) so a bounded run does not select only the airport examples at the
start of the file. Changing the seed or training size changes the oracle: reset or use
a new database. Reports contain the configuration needed to reproduce the selection.

After restarting the server, verify the same persisted graph without re-ingesting it:

```sh
go run ./demo/graph-web_nlg --verify-only
```

Use the same dataset, sample size, seed and database as the original run. `--pair-bytes`
only applies with `--reset-db`; an existing database keeps its stored stride.

## What is validated

1. Ingest **training only**: deduplicate complete triples, create nodes with their
   original RDF spelling in `props.rdf_value`, then batch directed edges of weight 1.
2. Check every node and its complete reference sentences, and every edge's endpoints,
   type, direction and weight against an independent client-side oracle.
3. Page through every node's outgoing **and incoming** adjacency using seven-edge pages,
   verifying exact sets, no duplicates, and cursor progress. Hubs also run filtered
   queries that exclude the first endpoint, catching skipped rows across page boundaries.
4. Rewrite the same edges concurrently while other clients verify typed, endpoint-anchored
   queries. Drain all workers, checkpoint, and repeat the complete oracle validation.
5. Recall from held-out sentences with bounded lexical expansion and two-hop traversal.
   Hydrate up to 32 outgoing edges from each of the top eight resolved seeds: relationships
   between seeds can be absent from recall's displayed paths. Check that every returned
   evidence edge belongs to the training graph.

RDF entity/literal spelling, case, punctuation, Unicode, quotes and underscores are
preserved. Node and relation IDs use reversible base64url tokens with different prefixes;
lossy slug normalization would merge distinct facts. Shared literal values remain shared
nodes, which deliberately creates hubs and stresses reverse adjacency.

Each node keeps its readable name and up to eight complete training reference sentences,
chosen in stable entry order. The report counts omitted references; no sentence is cut
in half. References describe the original entry's whole subgraph, not necessarily one edge.

The gated integration test additionally exercises two-hop queries, edge deletion and both
adjacency directions, restores the edge, and gracefully restarts the actual server to check
persistence. It runs on **both trie strides** with temporary data directories.

## Metrics and LLM evaluation

The JSON report includes ingest throughput (including node/reference writes), typed-query
p50/p95/p99 during rewrites, held-out text-retrieval latency (recall plus bounded seed adjacency), graph sizes and `SYSTEM_STATS`.
These are workload measurements, not universal engine throughput claims.

Validation/test facts can overlap training facts. The retrieval report explicitly separates:

- `training_coverage`: gold triples already present in the selected training graph;
- `known_triple_recall`: how many of those available facts appear in retrieved evidence;
- `all_gold_recall`: recovered gold triples including unavailable facts in the denominator;
- `evidence_precision`: retrieved facts belonging to this entry's gold subgraph;
- `truncated_queries`: recalls reaching engine traversal bounds;
- `seed_adjacency_truncated_pages`: seed-neighbor reads with more than the 32-edge page.

Only the first held-out reference sentence is queried per entry. Precision here is relevance
to that subgraph: an extra training fact can be true and still count as irrelevant. A zero
denominator reports zero; the raw counts distinguish that case. Unknown facts are not
invented. Retrieval quality is reported, not asserted to exceed an arbitrary threshold.
`cache=off` avoids mixing learned shortcuts into this baseline.

Each run also exports `*-cases.jsonl` with the sentence, gold triples, retrieved triples,
training provenance, an **extraction prompt** (sentence only), and a **generation prompt**
(retrieved facts only). Send only the appropriate prompt field to your LLM, not the whole
row containing gold answers. The harness makes no API calls and claims no LLM quality until
model outputs are supplied.

For exact RDF extraction evaluation, save one object per line:

```json
{"id":"validation/Airport/1/Id1","triples":[{"subject":"Aarhus_Airport","predicate":"cityServed","object":"Aarhus"}]}
```

Use actual IDs from the exported cases, then repeat the same run with:

```sh
go run ./demo/graph-web_nlg --verify-only --predictions /path/to/predictions.jsonl
```

The scorer reports micro precision/recall/F1 and entry exact match. Repeated triples are
sets; missing predictions count as empty, and unknown or duplicate IDs fail. Exact spelling
is required. This is an extraction metric, not a measure of prose fluency, factual entailment,
or open-world reasoning. No synthetic negative labels or NELL-style probability scores are used.

Reports and cases go to ignored `demo/graph-web_nlg/reports/` by default. Use
`--report-dir /path/to/results` or `--report-dir ''` to disable artifacts. Respect the
[dataset's own license and limitations](../../studies/datasets/web_nlg/README.md).

## Tests

```sh
go test ./demo/graph-web_nlg
CHEETAH_WEBNLG_E2E=1 go test -v -count=1 -timeout 15m ./demo/graph-web_nlg
# Also run a sampled slice of the checked-out real dataset:
CHEETAH_WEBNLG_E2E=1 CHEETAH_WEBNLG_REAL=1 \
  go test -v -count=1 -timeout 15m ./demo/graph-web_nlg
# A larger real training slice; 0 selects the full training split:
CHEETAH_WEBNLG_E2E=1 CHEETAH_WEBNLG_REAL=1 CHEETAH_WEBNLG_TRAIN=2000 \
  go test -v -count=1 -timeout 30m ./demo/graph-web_nlg
```

The test flags are read only by this client test harness. No running server, external
service, credentials or dataset download is needed. All runtime data lives in temporary
folders and is removed by the Go test runner.

## Recorded validation

On 2026-10-05, a local run with seed 42, 1,000 sampled training entries, 100 validation
entries, four workers and two rewrite rounds passed the complete graph oracle:
1,678 nodes, 1,714 unique edges and 3,428 stress edge rewrites. Measured ingest was
13.5 edges/s including reference indexing; typed-query p95 was 1.13 ms and text-retrieval
p95 was 253.05 ms. Training covered 61.0% of the held-out gold triples; the bounded retrieval
pipeline recovered 81.1% of those available triples. These are sampled validation results,
not full-corpus or LLM quality claims.

The work also exposed an engine bug: filtered graph queries could advance their cursor
past matching edges not yet returned. The fix in `src/graph.go` resumes from the last
consumed adjacency key, with `TestGraphQueryFilteredPagination` covering both trie strides.
