# Converting GrailQA SPARQL queries to Cheetah queries

This study maps **24 reproducibly sampled GrailQA development queries** to the Cheetah query
language implemented in this checkout. It supplies concrete commands, the result-processing steps
needed to preserve SPARQL meaning, and a development plan for a later converter and engine extensions.
It does not implement a converter or change the server.

The main finding is that Cheetah already has the necessary directed adjacency walks, anchored
endpoint joins and existence filters for these graph shapes. The remaining work is chiefly **RDF
term semantics, explicit variable bindings and projection, distinct/aggregate semantics, and a
reliable way to finish large joined results**. A syntactically valid GRAPH_QUERY is not, by itself,
an equivalent SPARQL query.

## 1. Evidence, sample and limits

- Dataset: `/Users/richard/Downloads/GrailQA_v1.0/grailqa_v1.0_dev.json`.
- Input field: `sparql_query`. Questions are quoted for context; `s_expression` and `graph_query`
  are not the conversion authority. In particular, generated graph metadata can use placeholder
  literal values for extrema; those placeholders must not become query constants.
- Dataset SHA-256: `55f3c2f937fee12bde3242247f5a06c6d595a898aa291b62fd3c2abf502f748c`.
- Dataset size: **6,763 rows**. File order and indexes below are zero-based.
- Inspected server revision: `fdb740e0d24db8277b9d54bc0a3c001f640e77c2`.
- Study date: 2026-11-03. Commands describe the current source, not a proposed future grammar.
- Selection: 20 uniform random rows without replacement, seed `20261103`, sorted by input index;
  plus four explicitly identified random coverage supplements, selected using the same seed reset
  for each eligible pool. The supplements are not a second uniform sample of the whole dataset.

Reproduce the selection with Python's standard library:

```python
import json
import random

with open('/Users/richard/Downloads/GrailQA_v1.0/grailqa_v1.0_dev.json') as f:
    rows = json.load(f)
seed = 20261103
main = sorted(random.Random(seed).sample(range(len(rows)), 20))
pools = {
    'count': [i for i, d in enumerate(rows) if d['function'] == 'count'],
    'comparison': [i for i, d in enumerate(rows) if d['function'] in ('<', '<=', '>', '>=')],
    'dateTime': [i for i, d in enumerate(rows) if 'XMLSchema#dateTime' in d['sparql_query']],
    'complex comparison': [i for i, d in enumerate(rows)
                           if d['function'] == '<=' and d['num_edge'] >= 3],
}
extra = [random.Random(seed).choice(pool) for pool in pools.values()]
print(main)
print(extra)  # [628, 1107, 1249, 913]
```

The coverage pools contain 440, 303, 3 and 28 rows respectively. This is a translation/design study,
not a benchmark: the Freebase RDF graph is not supplied by the question/answer JSON and has not been
imported here. Recorded answers below are dataset expectations, not answers produced against a
complete Cheetah import. No dataset accuracy or runtime claim follows from this sample.

A local census of the complete JSON gives the following `function` distribution:

| Function metadata | Rows |
| --- | ---: |
| none | 5,512 |
| count | 440 |
| argmax | 298 |
| argmin | 210 |
| <= | 107 |
| >= | 93 |
| < | 57 |
| > | 46 |

A text census found no `UNION` or `OPTIONAL` tokens in this file's SPARQL strings. This does not
establish full SPARQL coverage. Literal datatype mentions include float (622), date (104), integer
(94), gYear (20), gYearMonth (10), and dateTime (3); these are occurrence counts, not query counts.
The 24 examples deliberately expose several such semantics rather than claiming to cover every form.

## 2. Import contract assumed by the translations

The JSON supplies queries and expected answers, not the underlying triples. A future converter needs
an explicit, versioned contract with a Freebase importer before its output can be executed usefully.
The following is a **proposed client data convention**, supported by existing graph storage; the
server does not currently enforce an RDF schema.

| RDF component | Cheetah representation used here |
| --- | --- |
| `http://rdf.freebase.com/ns/m.0vs7y5v` | Node ID `m.0vs7y5v` |
| Freebase class such as `boats.ship_class` | Ordinary node ID `boats.ship_class` |
| Triple `s :p o` | Directed edge `from=s to=o type=p` |
| `s :type.object.type :Class` | Explicit edge `s -> Class` of type `type.object.type` |
| Literal object | A stable literal-node ID with complete RDF term metadata in node props |
| Mediator / CVT | Ordinary node with its own ID and incident edges; keep all bindings |
| Human-readable entity name | Optional display metadata; never replace the ID with the name |

Expand prefixes in the parser, then compact only known namespaces reversibly. Keep other IRIs in a
collision-free, reversible encoding; `m.*`, `g.*`, class names and predicate names are opaque IDs.
Do not infer inverse predicates or relationships from English names. Dots in an edge type are not
property traversal. A Freebase `type.object.type` edge is not automatically the same as `rdf:type`;
any such mapping must be declared by the importer.

Preserve **every** type membership as an edge. No single-class assumption is safe. Labels could
mirror type membership for faster filters, but the commands below use explicit type edges so they
need no label synchronization or inference. The graph stores one directed edge per endpoint/type
triple; repeated identical RDF triples collapse naturally. Multiple distinct literal values must
remain separate objects, not overwrite one scalar property on the subject.

For literal nodes, use a reserved client namespace outside Cheetah's internal `\x01`…`\x08` range,
for example `lit:<sha256>`, and retain at least:

```json
{
  "rdf_kind": "literal",
  "lexical": "32.0",
  "datatype": "http://www.w3.org/2001/XMLSchema#float",
  "language": ""
}
```

For the concrete literal IDs in cases 07, 19 and 23, define this exact versioned codec (with collision
detection against stored metadata):

```python
import hashlib
import json

def literal_id(lexical, datatype, language=''):
    raw = json.dumps(['rdf-term-v1', 'literal', lexical, datatype, language.lower()],
                     ensure_ascii=False, separators=(',', ':')).encode('utf-8')
    return 'lit:' + hashlib.sha256(raw).hexdigest()
```

Preserve decoded lexical text without Unicode/value normalization. Entity IRIs and blank nodes also
need explicit kinds if the importer accepts them. Preserve lexical identity separately from normalized
value keys: `"32.0"^^xsd:float` and another numeric spelling can have equal values without being the
same RDF term. **VALUES and shared-variable joins require term identity; FILTER `=` may compare
values.** These are different operations. See the W3C definitions of
[compatible mappings](https://www.w3.org/TR/sparql11-query/#defn_algCompatibleMapping) and
[operator mapping](https://www.w3.org/TR/sparql11-query/#OperatorMapping).
An exact literal hash is therefore the right anchor for these singleton VALUES cases. Decode xsd:float at
its declared precision, not as an arbitrary decimal or always-float64 value. Do not silently coerce
integer, date, gYear, gYearMonth and dateTime to one string or one floating-point field.

Literal props can carry derived numeric or temporal fields for safe predicate pushdown. Keep
original metadata authoritative, account for timezone and missing-timezone semantics, and recheck
with the RDF evaluator. Cheetah's JSON numbers and graph WHERE comparisons alone do not implement
the RDF datatype system. In particular, string ordering is bytewise and numeric comparison can
coerce property values; neither is a general SPARQL comparator.
Record the reference endpoint's datatype/ordering extensions, especially xsd:date and partial dates,
rather than assuming every temporal datatype has interoperable ordering. Record its literal
canonicalization and entailment policy too; compare equivalent representations of the same RDF input.

Example import using existing commands (base64 placeholder must be replaced by an actual JSON encoding):

```text
GRAPH_NODE_SET id=boats.ship_class
GRAPH_NODE_SET id=m.example_ship
GRAPH_NODE_SET id=lit:example_float props=<base64-json-of-the-literal-metadata>
GRAPH_EDGE_SET from=m.example_ship to=boats.ship_class type=type.object.type directed=1
GRAPH_EDGE_SET from=m.example_ship to=lit:example_float type=boats.ship_class.max_speed_knots directed=1
```

The example IDs are illustrative. An importer should encode arguments with the binder's argument
codec; it must not concatenate arbitrary RDF text into protocol tokens. Scores, confidence,
modality, recall decay and lexical term-index guesses do not affect exact RDF triple matching.
Use deterministic GRAPH_QUERY, not GRAPH_RECALL or prediction ranking, for these translations.

## 3. What the present language can actually express

These findings come from [the parser](../src/graph_query_parse.go),
[expression evaluation](../src/graph_query_expr.go),
[the streaming relational executor](../src/graph_query.go),
[graph storage and dispatch](../src/graph.go), and
[the existing query tests](../src/graph_query_test.go). The
[README grammar](../README.md#pattern-queries--graph_query) is a useful operator reference.

| Need | Current behavior | Conversion consequence |
| --- | --- | --- |
| Start from a known entity | MATCH requires a concrete left ID | Reverse an edge to anchor on a constant when useful |
| Start from a class | A class is a node ID under the import contract | `MATCH (id='Class')<-[:type.object.type]-(*)` enumerates members through adjacency |
| Mixed relation/direction paths | Endpoint JOIN, with incoming and outgoing arrows | Bind `join1`, `join2`, etc.; do not use HOPS for heterogeneous path predicates |
| Relationship conjunction | INCLUDE is an existence filter | Useful for type checks and fixed-constant intersections; it does not multiply rows |
| Scalar comparison and booleans | WHERE supports comparisons, IN, NOT/AND/OR | Safe pushdown for field-to-constant filters with matching types |
| Field-to-field comparison | Explicit `@field` RHS resolves another bound scalar | Use `from.id != @join1.to.id`; bare `join1.to.id` remains a legacy literal |
| Named SPARQL variables | `RETURN field AS alias` selects bound fields | Maintain a compiler binding map; endpoints stay edge-oriented even on incoming walks |
| Projection | `RETURN [DISTINCT] field AS value` hydrates/project fields; INCLUDE FIELDS still selects JSON paths | Native aliases/global DISTINCT can replace client projection when every RDF filter is already applied |
| RETURN nodes | Unique endpoints of base MATCH edges | Includes the anchor and does not select the answer variable from joined edges |
| RETURN count | Legacy mode counts matched rows until LIMIT; `RETURN COUNT(*)` / `COUNT(DISTINCT field)` consume full input | Use aggregate expressions for exact count; handle explicit budget/capacity errors |
| GROUP BY / aggregates | Legacy groups retain weight aggregates; `MIN(field)` / `MAX(field)` accept uniform JSON numbers or strings | RDF typed extrema and term-identity join-back still belong in the converter |
| Paging | Single-hop scans/filters/projected edges have consumed-row cursors | Drain every page; the first page is not the answer set |
| Joined results | Authenticated full-path cursors and explicit `complete=true|false` | Resume until complete; graph mutation, changed query or reopen invalidates the token |
| Temporal/numeric RDF types | No native RDF evaluator | Import metadata and evaluate accurately in a converter, then consider native extensions |

Bounds are operational limits, not SPARQL semantics. Default LIMIT is 128 and the maximum is 2,048;
BUDGET defaults to 16,384 and has a maximum of 1,048,576 work units. There are at most eight
JOIN/INCLUDE/EXCLUDE relations and 32 projected fields; retained row JSON is capped at 8 MiB.
GROUP BY has additional group/key ceilings and errors on incomplete aggregation. Global DISTINCT
and COUNT DISTINCT share a 65,536-entry / 8 MiB state ceiling; DISTINCT pages recompute the full
bounded set before slicing. Exact aggregate expressions ignore LIMIT on their input. JOIN and other
relational clauses use HOPS 1. HOPS applies a repeated relation pattern and retains bounded traversal
semantics; it is not a substitute for a basic graph pattern with independently bound variables.

All commands in the case studies below are on **one protocol line**, use current grammar and
explicit large bounds, and select just the fields the adapter needs. The commands containing JOIN
are **candidate enumerators** whose native join pages can now be exhausted with CURSOR and
`complete`. Their original low-level projections and residual RDF filters are deliberately retained
as a reproducible manifest. Section 6 shows the new native answer/count forms. Raising LIMIT/BUDGET
alone does not remove their maximums. Result pagination, post-filters, typed evaluation and aggregation are
part of the conversion, as specified separately for each case.

### Status key and sample manifest

- **P**: the graph selection can be enumerated with current single-hop paging; a client still adapts
  the manifest's low-level result (projection, distinct or count), or emits the native forms in
  section 6. There are 17 such cases, including three exact literal
  VALUES lookups under the proposed term codec.
- **J**: joined candidates require bound comparisons and exhaustive paging (both now native); the
  manifest retains its original adapter-oriented projection.
- **T**: typed literal semantics require an RDF-aware adapter or future engine support.
- **A**: a complete typed aggregate plus equality join back to the winners is required.

The other seven cases have one or more of J/T/A. These are design classifications, not measured
success rates on the Freebase graph.

| Case | Input index | qid | Level | Selection | Shape |
| --- | ---: | --- | --- | --- | --- |
| 01 | 447 | 2102557006000 | zero-shot | uniform | P |
| 02 | 732 | 2100567016000 | i.i.d. | uniform | P |
| 03 | 937 | 2102120001000 | compositional | uniform | P |
| 04 | 1094 | 2100283000000 | zero-shot | uniform | P |
| 05 | 1176 | 3205555001000 | zero-shot | uniform | J |
| 06 | 1328 | 2100376022000 | zero-shot | uniform | P |
| 07 | 1335 | 2103004004000 | compositional | uniform | P |
| 08 | 1460 | 2104903001000 | compositional | uniform | A |
| 09 | 1590 | 2101404009000 | zero-shot | uniform | P |
| 10 | 2080 | 3205528004000 | zero-shot | uniform | P |
| 11 | 3709 | 2100866002000 | zero-shot | uniform | P |
| 12 | 4164 | 2100010016000 | i.i.d. | uniform | P |
| 13 | 4331 | 2102735001000 | zero-shot | uniform | P |
| 14 | 4339 | 3200480001000 | compositional | uniform | J |
| 15 | 5041 | 2101739016000 | compositional | uniform | P |
| 16 | 5077 | 4303158002000 | zero-shot | uniform | A |
| 17 | 5577 | 2100654019000 | zero-shot | uniform | P |
| 18 | 5758 | 2101825013000 | zero-shot | uniform | P |
| 19 | 6059 | 2100528009000 | zero-shot | uniform | P |
| 20 | 6684 | 3201177013000 | i.i.d. | uniform | J |
| 21 | 628 | 2103336005000 | zero-shot | count supplement | P |
| 22 | 1107 | 2104544018000 | compositional | comparison supplement | T |
| 23 | 1249 | 2100967016000 | i.i.d. | dateTime supplement | P |
| 24 | 913 | 4301069003000 | zero-shot | complex comparison supplement | T/J |

## 4. Query-by-query translations

Each source block preserves the selected `sparql_query` text, with its repeated PREFIX line
removed for readability. The prefixes are identical in all 24 selected rows:

```sparql
PREFIX rdf: <http://www.w3.org/1999/02/22-rdf-syntax-ns#>
PREFIX rdfs: <http://www.w3.org/2000/01/rdf-schema#>
PREFIX : <http://rdf.freebase.com/ns/>
```

### 01. chrome engine 6 is the successor to which video game engine?

Index **447**, qid **2102557006000**, level `zero-shot`. Status: **P — paged traversal plus result adaptation**.

**Meaning.** Find engines whose successor is Chrome Engine 6. The answer is the predecessor, so traverse incoming successor edges.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :cvg.computer_game_engine . 
VALUES ?x1 { :m.0vs7y5v } 
?x0 :cvg.computer_game_engine.successor_engine ?x1 . 
FILTER ( ?x0 != ?x1  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='m.0vs7y5v')<-[:cvg.computer_game_engine.successor_engine]-(*) INCLUDE (from)-[:type.object.type]->(id='cvg.computer_game_engine') WHERE from.id != 'm.0vs7y5v' RETURN edges INCLUDE FIELDS from LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x0 = from`; `?x1 = m.0vs7y5v`.

**Finish / missing support.** Drain every page, take each `from`, deduplicate, and emit it as `value`. 

**Dataset expectation:** `m.0gyg3cf` (Chrome Engine 5). 1 recorded answer(s).

### 02. the ε per a is in what star system?

Index **732**, qid **2100567016000**, level `i.i.d.`. Status: **P — paged traversal plus result adaptation**.

**Meaning.** Find star systems containing the specified star; start at the star and traverse incoming `stars` edges.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :astronomy.star_system . 
VALUES ?x1 { :m.0x0lcp5 } 
?x0 :astronomy.star_system.stars ?x1 . 
FILTER ( ?x0 != ?x1  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='m.0x0lcp5')<-[:astronomy.star_system.stars]-(*) INCLUDE (from)-[:type.object.type]->(id='astronomy.star_system') WHERE from.id != 'm.0x0lcp5' RETURN edges INCLUDE FIELDS from LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x0 = from`; `?x1 = m.0x0lcp5`.

**Finish / missing support.** Drain every page, take each `from`, deduplicate, and emit it as `value`. 

**Dataset expectation:** `m.0270q0g` (Epsilon Persei). 1 recorded answer(s).

### 03. east asian religions is a religion based on what?

Index **937**, qid **2102120001000**, level `compositional`. Status: **P — paged traversal plus result adaptation**.

**Meaning.** Find beliefs belonging to the supplied East Asian religions entity, restricted to `religion.belief`.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :religion.belief . 
VALUES ?x1 { :m.028b9mc } 
?x0 :religion.belief.belief_of ?x1 . 
FILTER ( ?x0 != ?x1  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='m.028b9mc')<-[:religion.belief.belief_of]-(*) INCLUDE (from)-[:type.object.type]->(id='religion.belief') WHERE from.id != 'm.028b9mc' RETURN edges INCLUDE FIELDS from LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x0 = from`; `?x1 = m.028b9mc`.

**Finish / missing support.** Drain every page, take each `from`, deduplicate, and emit it as `value`. 

**Dataset expectation:** `m.07kzs` (Tao); `m.027k36f` (De). 2 recorded answer(s).

### 04. we remember paleontologist and evolutionary biologist stephen jay gould is a segment on which radio program episode?

Index **1094**, qid **2100283000000**, level `zero-shot`. Status: **P — paged traversal plus result adaptation**.

**Meaning.** Find radio episodes containing the specified segment. Keep the full relation name; the segment title is display text.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :radio.radio_program_episode . 
VALUES ?x1 { :m.0bjd8hq } 
?x0 :radio.radio_program_episode.segments ?x1 . 
FILTER ( ?x0 != ?x1  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='m.0bjd8hq')<-[:radio.radio_program_episode.segments]-(*) INCLUDE (from)-[:type.object.type]->(id='radio.radio_program_episode') WHERE from.id != 'm.0bjd8hq' RETURN edges INCLUDE FIELDS from LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x0 = from`; `?x1 = m.0bjd8hq`.

**Finish / missing support.** Drain every page, take each `from`, deduplicate, and emit it as `value`. 

**Dataset expectation:** `m.0bjd5_n` (Fresh Air: May 21, 2002). 1 recorded answer(s).

### 05. what is the unit used for frequency in the measurement system for millisiemens?

Index **1176**, qid **3205555001000**, level `zero-shot`. Status: **J — joined candidates; native inequality and continuation available**.

**Meaning.** Find frequency units in the same measurement system as millisiemens. The shared system is a join variable, not the answer.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :measurement_unit.unit_of_frequency . 
?x1 :type.object.type :measurement_unit.measurement_system . 
VALUES ?x2 { :m.0gvb8ml } 
?x0 :measurement_unit.unit_of_frequency.measurement_system ?x1 . 
?x2 :measurement_unit.conductance_unit.measurement_system ?x1 . 
FILTER ( ?x0 != ?x1 && ?x0 != ?x2 && ?x1 != ?x2  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='m.0gvb8ml')-[:measurement_unit.conductance_unit.measurement_system]->(*) INCLUDE (to)-[:type.object.type]->(id='measurement_unit.measurement_system') JOIN (to)<-[:measurement_unit.unit_of_frequency.measurement_system]-(*) INCLUDE (join1.from)-[:type.object.type]->(id='measurement_unit.unit_of_frequency') WHERE to.id != 'm.0gvb8ml' AND join1.from.id != 'm.0gvb8ml' RETURN rows INCLUDE FIELDS edge.to,join1.from LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x2 = m.0gvb8ml`; `?x1 = edge.to`; `?x0 = join1.from`.

**Finish / missing support.** Post-filter `?x0 != ?x1`, then project/deduplicate `join1.from`. The two comparisons against `?x2` are already pushed into WHERE. Alternatively add `to.id != @join1.from.id` to WHERE and use `RETURN DISTINCT join1.from.id AS value` (remove INCLUDE FIELDS). Drain the native continuation; `complete=true` now certifies exhausted output. Distinct/work capacity errors require an explicit client fallback.

**Dataset expectation:** `m.03nsh` (Hertz); `m.02n9swl` (Megahertz); `m.04j3wb3` (Kilohertz); `g.121lm_nv` (Gigahertz). 4 recorded answer(s).

### 06. the ios app store gave what video game rating?

Index **1328**, qid **2100376022000**, level `zero-shot`. Status: **P — paged traversal plus result adaptation**.

**Meaning.** Find game-rating entities belonging to the iOS App Store rating system; values such as `4+` are names of entities here.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :cvg.computer_game_rating . 
VALUES ?x1 { :m.03wc248 } 
?x0 :cvg.computer_game_rating.rating_system ?x1 . 
FILTER ( ?x0 != ?x1  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='m.03wc248')<-[:cvg.computer_game_rating.rating_system]-(*) INCLUDE (from)-[:type.object.type]->(id='cvg.computer_game_rating') WHERE from.id != 'm.03wc248' RETURN edges INCLUDE FIELDS from LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x0 = from`; `?x1 = m.03wc248`.

**Finish / missing support.** Drain every page, take each `from`, deduplicate, and emit it as `value`. 

**Dataset expectation:** `m.0hh76vc` (4+); `m.0hh76vn` (9+); `m.0hh76vw` (12+); `m.0hh76w7` (17+). 4 recorded answer(s).

### 07. what is the class of ship that has a maximum speed of 32.0?

Index **1335**, qid **2103004004000**, level `compositional`. Status: **P — exact literal-term anchor plus paged result adaptation**.

**Meaning.** Find ship classes pointing to the exact RDF term `"32.0"^^xsd:float` bound by VALUES. Do not rewrite this into a numeric FILTER that could admit other lexical terms.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :boats.ship_class . 
VALUES ?x1 { "32.0"^^<http://www.w3.org/2001/XMLSchema#float> } 
?x0 :boats.ship_class.max_speed_knots ?x1 . 
FILTER ( ?x0 != ?x1  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='lit:05cf8d24db4ecfc813a5d2ac3a69dac17a010ac8bd8912524c727e5b1a948ac4')<-[:boats.ship_class.max_speed_knots]-(*) INCLUDE (from)-[:type.object.type]->(id='boats.ship_class') RETURN edges INCLUDE FIELDS from LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x0 = from`; `?x1 = lit:05cf8d24db4ecfc813a5d2ac3a69dac17a010ac8bd8912524c727e5b1a948ac4` represents `32.0` with datatype `http://www.w3.org/2001/XMLSchema#float`.

**Finish / missing support.** Compute the literal-node ID with the exact term codec in section 2, drain every page, and distinct-project `from` as `value`. VALUES joins on RDF term identity: a differently spelled literal with the same numeric value or timestamp is not an additional match. The source inequality is true for an RDF subject IRI/blank node versus this literal under the import contract. No numeric/temporal value evaluator is needed for this particular VALUES lookup.

**Dataset expectation:** `m.0f3rpg` (Sovremenny-class destroyer). 1 recorded answer(s).

### 08. which disney rides opened recently?

Index **1460**, qid **2104903001000**, level `compositional`. Status: **A — global typed aggregate, ties, and completeness required**.

**Meaning.** Find all Disney rides pointing to the RDF term selected by the global MAX opening-value subquery. The source returns every ride sharing that selected term, including ties.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :amusement_parks.disney_ride . 
{
SELECT (MAX(?y1) AS ?x1)  WHERE { 
?y0 :type.object.type :amusement_parks.disney_ride . 
?y0 :amusement_parks.ride.opened ?y1 . 
FILTER ( ?y0 != ?y1  )
}
}
?x0 :amusement_parks.ride.opened ?x1 . 
FILTER ( ?x0 != ?x1  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='amusement_parks.disney_ride')<-[:type.object.type]-(*) JOIN (from)-[:amusement_parks.ride.opened]->(*) RETURN rows INCLUDE FIELDS edge.from,join1.to LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x0 / ?y0 = edge.from`; opening values `?x1 / ?y1 = join1.to`.

**Finish / missing support.** Exhaust the complete candidate relation, apply the inner FILTER, compute MAX using SPARQL aggregate ordering, then retain outer rows pointing to the selected RDF term and satisfying the outer FILTER. Deduplicate `edge.from`. Do not use LIMIT 1: the source lists two tied winners. Equal values with different lexical terms are not automatically join-compatible. Native MIN/MAX can aggregate a selected JSON property, but do not supply SPARQL temporal ordering or select an RDF term for this join-back.

**Dataset expectation:** `m.09v0nvf` (Red Car Trolley); `m.03ck7tz` (Radiator Springs Racers). 2 recorded answer(s).

### 09. which video game version has the peripherals supported by kinect for xbox one?

Index **1590**, qid **2101404009000**, level `zero-shot`. Status: **P — paged traversal plus result adaptation**.

**Meaning.** Find game versions supporting the specified Kinect peripheral; preserve the direction from version to peripheral.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :cvg.game_version . 
VALUES ?x1 { :m.0100rc1z } 
?x0 :cvg.game_version.peripherals_supported ?x1 . 
FILTER ( ?x0 != ?x1  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='m.0100rc1z')<-[:cvg.game_version.peripherals_supported]-(*) INCLUDE (from)-[:type.object.type]->(id='cvg.game_version') WHERE from.id != 'm.0100rc1z' RETURN edges INCLUDE FIELDS from LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x0 = from`; `?x1 = m.0100rc1z`.

**Finish / missing support.** Drain every page, take each `from`, deduplicate, and emit it as `value`. 

**Dataset expectation:** `m.010vxhkm` (Dance Central Spotlight). 1 recorded answer(s).

### 10. who designed both lost planet: extreme condition and megaman 8?

Index **2080**, qid **3205528004000**, level `zero-shot`. Status: **P — paged intersection plus result adaptation**.

**Meaning.** Intersect the designers of the two specified games, retaining only `cvg.cvg_designer` entities.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :cvg.cvg_designer . 
VALUES ?x1 { :m.09ydd5 } 
VALUES ?x2 { :m.05s6qh } 
?x1 :cvg.computer_videogame.designers ?x0 . 
?x2 :cvg.computer_videogame.designers ?x0 . 
FILTER ( ?x0 != ?x1 && ?x0 != ?x2 && ?x1 != ?x2  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='m.09ydd5')-[:cvg.computer_videogame.designers]->(*) INCLUDE (to)-[:type.object.type]->(id='cvg.cvg_designer') INCLUDE (to)<-[:cvg.computer_videogame.designers]-(id='m.05s6qh') WHERE to.id NOT IN ('m.09ydd5','m.05s6qh') RETURN edges INCLUDE FIELDS to LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x1 = m.09ydd5`; `?x2 = m.05s6qh`; `?x0 = to`.

**Finish / missing support.** The fixed constants are unequal, so their mutual FILTER is true. The existence probe implements the second conjunct without multiplying rows. Drain pages, deduplicate `to`, and emit `value`. This reduction is safe because the source ultimately returns distinct designers.

**Dataset expectation:** `m.04_mvx` (Keiji Inafune). 1 recorded answer(s).

### 11. what measurement system has a luminous flux units of lm?

Index **3709**, qid **2100866002000**, level `zero-shot`. Status: **P — paged traversal plus result adaptation**.

**Meaning.** Find measurement systems whose luminous-flux units include the specified lumen entity.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :measurement_unit.measurement_system . 
VALUES ?x1 { :m.04ckf7 } 
?x0 :measurement_unit.measurement_system.luminous_flux_units ?x1 . 
FILTER ( ?x0 != ?x1  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='m.04ckf7')<-[:measurement_unit.measurement_system.luminous_flux_units]-(*) INCLUDE (from)-[:type.object.type]->(id='measurement_unit.measurement_system') WHERE from.id != 'm.04ckf7' RETURN edges INCLUDE FIELDS from LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x0 = from`; `?x1 = m.04ckf7`.

**Finish / missing support.** Drain every page, take each `from`, deduplicate, and emit it as `value`. 

**Dataset expectation:** `m.0c13h` (International System of Units). 1 recorded answer(s).

### 12. in athens, greece, what olympic games were hosted?

Index **4164**, qid **2100010016000**, level `i.i.d.`. Status: **P — paged traversal plus result adaptation**.

**Meaning.** Find Olympic Games hosted in Athens, preserving all matching events rather than selecting one event by its label.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :olympics.olympic_games . 
VALUES ?x1 { :m.0n2z } 
?x0 :olympics.olympic_games.host_city ?x1 . 
FILTER ( ?x0 != ?x1  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='m.0n2z')<-[:olympics.olympic_games.host_city]-(*) INCLUDE (from)-[:type.object.type]->(id='olympics.olympic_games') WHERE from.id != 'm.0n2z' RETURN edges INCLUDE FIELDS from LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x0 = from`; `?x1 = m.0n2z`.

**Finish / missing support.** Drain every page, take each `from`, deduplicate, and emit it as `value`. 

**Dataset expectation:** `m.0130xg` (1906 Intercalated Games); `m.0c_tl` (1896 Summer Olympics); `m.0kbvb` (2004 Summer Olympics). 3 recorded answer(s).

### 13. the southwest academic conference conference is what type of conference?

Index **4331**, qid **2102735001000**, level `zero-shot`. Status: **P — paged traversal plus result adaptation**.

**Meaning.** Find the conference-type entities that include the supplied conference.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :conferences.type_of_conference . 
VALUES ?x1 { :m.03qnbsz } 
?x0 :conferences.type_of_conference.conferences_of_this_type ?x1 . 
FILTER ( ?x0 != ?x1  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='m.03qnbsz')<-[:conferences.type_of_conference.conferences_of_this_type]-(*) INCLUDE (from)-[:type.object.type]->(id='conferences.type_of_conference') WHERE from.id != 'm.03qnbsz' RETURN edges INCLUDE FIELDS from LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x0 = from`; `?x1 = m.03qnbsz`.

**Finish / missing support.** Drain every page, take each `from`, deduplicate, and emit it as `value`. 

**Dataset expectation:** `m.07bx10` (Academic conference). 1 recorded answer(s).

### 14. what's the status of the websites owned by us office of personnel management?

Index **4339**, qid **3200480001000**, level `compositional`. Status: **J — reversed two-hop candidates; native projection/comparison available**.

**Meaning.** Find the statuses of websites owned by the supplied agency. The status-to-site relation points into the intermediate website.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :internet.website_status . 
?x1 :type.object.type :internet.website . 
VALUES ?x2 { :m.044s1w } 
?x0 :internet.website_status.sites ?x1 . 
?x1 :internet.website.owner ?x2 . 
FILTER ( ?x0 != ?x1 && ?x0 != ?x2 && ?x1 != ?x2  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='m.044s1w')<-[:internet.website.owner]-(*) INCLUDE (from)-[:type.object.type]->(id='internet.website') JOIN (from)<-[:internet.website_status.sites]-(*) INCLUDE (join1.from)-[:type.object.type]->(id='internet.website_status') WHERE from.id != 'm.044s1w' AND join1.from.id != 'm.044s1w' RETURN rows INCLUDE FIELDS edge.from,join1.from LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x2 = m.044s1w`; `?x1 = edge.from`; `?x0 = join1.from`.

**Finish / missing support.** Post-filter `?x0 != ?x1`, then deduplicate `join1.from`. Multiple websites can yield the same status. Preserve the intermediate binding until all FILTERs have been checked; projection before filtering would lose needed information. Alternatively add `from.id != @join1.from.id` and use `RETURN DISTINCT join1.from.id AS value` without INCLUDE FIELDS; exhaust native continuation or handle a bounded-state error with client expansion.

**Dataset expectation:** `m.02hsn4g` (Active). 1 recorded answer(s).

### 15. carotene has been tested for in which medical trial?

Index **5041**, qid **2101739016000**, level `compositional`. Status: **P — paged traversal plus result adaptation**.

**Meaning.** Find medical-trial records whose tested treatment is the supplied carotene entity. This is a graph lookup, not a claim about current trial evidence.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :medicine.medical_trial . 
VALUES ?x1 { :m.0204v } 
?x0 :medicine.medical_trial.treatment_being_tested ?x1 . 
FILTER ( ?x0 != ?x1  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='m.0204v')<-[:medicine.medical_trial.treatment_being_tested]-(*) INCLUDE (from)-[:type.object.type]->(id='medicine.medical_trial') WHERE from.id != 'm.0204v' RETURN edges INCLUDE FIELDS from LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x0 = from`; `?x1 = m.0204v`.

**Finish / missing support.** Drain every page, take each `from`, deduplicate, and emit it as `value`. 

**Dataset expectation:** `m.03zbbx9` (Physicians' Health Study). 1 recorded answer(s).

### 16. the aircraft model with a minimum cost of <3> was designed by whom?

Index **5077**, qid **4303158002000**, level `zero-shot`. Status: **A — mediator joins, dynamic inequalities, global MIN and completeness**.

**Meaning.** Find designers connected to aircraft models whose monetary-range minimum reaches the global minimum over the complete constrained designer/model/range path. Keep the monetary-range mediator.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :aviation.aircraft_designer . 
?x1 :type.object.type :aviation.aircraft_model . 
?x2 :type.object.type :measurement_unit.monetary_range . 
{
SELECT (MIN(?y3) AS ?x3)  WHERE { 
?y0 :type.object.type :aviation.aircraft_designer . 
?y1 :type.object.type :aviation.aircraft_model . 
?y2 :type.object.type :measurement_unit.monetary_range . 
?y1 :aviation.aircraft_model.designed_by ?y0 . 
?y1 :aviation.aircraft_model.unit_cost ?y2 . 
?y2 :measurement_unit.monetary_range.min_value ?y3 . 
FILTER ( ?y0 != ?y1 && ?y0 != ?y2 && ?y0 != ?y3 && ?y1 != ?y2 && ?y1 != ?y3 && ?y2 != ?y3  )
}
}
?x1 :aviation.aircraft_model.designed_by ?x0 . 
?x1 :aviation.aircraft_model.unit_cost ?x2 . 
?x2 :measurement_unit.monetary_range.min_value ?x3 . 
FILTER ( ?x0 != ?x1 && ?x0 != ?x2 && ?x0 != ?x3 && ?x1 != ?x2 && ?x1 != ?x3 && ?x2 != ?x3  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='aviation.aircraft_designer')<-[:type.object.type]-(*) JOIN (from)<-[:aviation.aircraft_model.designed_by]-(*) INCLUDE (join1.from)-[:type.object.type]->(id='aviation.aircraft_model') JOIN (join1.from)-[:aviation.aircraft_model.unit_cost]->(*) INCLUDE (join2.to)-[:type.object.type]->(id='measurement_unit.monetary_range') JOIN (join2.to)-[:measurement_unit.monetary_range.min_value]->(*) RETURN rows INCLUDE FIELDS edge.from,join1.from,join2.to,join3.to LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x0 / ?y0 = edge.from` (designer); `?x1 / ?y1 = join1.from` (model); `?x2 / ?y2 = join2.to` (range); `?x3 / ?y3 = join3.to` (literal).

**Finish / missing support.** Evaluate every original pairwise inequality, compute MIN using SPARQL aggregate ordering over the inner relation's `?y3` terms, then retain outer rows with that exact selected term and distinct-project designers. This is a global path minimum, not a per-designer minimum followed by a heuristic ranking. A missing cost is an absent inner-join row, not zero. Keep all designers sharing the selected term, even though the recorded answer has one.

**Dataset expectation:** `m.01jp40` (Geoffrey de Havilland). 1 recorded answer(s).

### 17. tonsley railway line is included in which rail network?

Index **5577**, qid **2100654019000**, level `zero-shot`. Status: **P — paged traversal plus result adaptation**.

**Meaning.** Find rail networks containing the specified railway line.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :rail.rail_network . 
VALUES ?x1 { :m.08lfrr } 
?x0 :rail.rail_network.railways ?x1 . 
FILTER ( ?x0 != ?x1  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='m.08lfrr')<-[:rail.rail_network.railways]-(*) INCLUDE (from)-[:type.object.type]->(id='rail.rail_network') WHERE from.id != 'm.08lfrr' RETURN edges INCLUDE FIELDS from LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x0 = from`; `?x1 = m.08lfrr`.

**Finish / missing support.** Drain every page, take each `from`, deduplicate, and emit it as `value`. 

**Dataset expectation:** `m.04rydm` (TransAdelaide). 1 recorded answer(s).

### 18. argonaut games created which video game engine?

Index **5758**, qid **2101825013000**, level `zero-shot`. Status: **P — paged traversal plus result adaptation**.

**Meaning.** Find game engines whose developer is the specified Argonaut Games entity.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :cvg.computer_game_engine . 
VALUES ?x1 { :m.04630n } 
?x0 :cvg.computer_game_engine.developer ?x1 . 
FILTER ( ?x0 != ?x1  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='m.04630n')<-[:cvg.computer_game_engine.developer]-(*) INCLUDE (from)-[:type.object.type]->(id='cvg.computer_game_engine') WHERE from.id != 'm.04630n' RETURN edges INCLUDE FIELDS from LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x0 = from`; `?x1 = m.04630n`.

**Finish / missing support.** Drain every page, take each `from`, deduplicate, and emit it as `value`. 

**Dataset expectation:** `g.12176kj4` (BRender). 1 recorded answer(s).

### 19. 1024.0 rate in bits per second corresponds to which unit of data transmission?

Index **6059**, qid **2100528009000**, level `zero-shot`. Status: **P — exact literal-term anchor plus paged result adaptation**.

**Meaning.** Find transmission-rate units pointing to the exact literal `"1024.0"^^xsd:float`. VALUES is a term-binding join, not a request to find approximately equivalent named units.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :measurement_unit.unit_of_data_transmission_rate . 
VALUES ?x1 { "1024.0"^^<http://www.w3.org/2001/XMLSchema#float> } 
?x0 :measurement_unit.unit_of_data_transmission_rate.rate_in_bits_per_second ?x1 . 
FILTER ( ?x0 != ?x1  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='lit:ebbb33ffa875519da78d260258729c6d4aa35709fa1cf8dc0633cd94a7735f18')<-[:measurement_unit.unit_of_data_transmission_rate.rate_in_bits_per_second]-(*) INCLUDE (from)-[:type.object.type]->(id='measurement_unit.unit_of_data_transmission_rate') RETURN edges INCLUDE FIELDS from LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x0 = from`; `?x1 = lit:ebbb33ffa875519da78d260258729c6d4aa35709fa1cf8dc0633cd94a7735f18` represents `1024.0` with datatype `http://www.w3.org/2001/XMLSchema#float`.

**Finish / missing support.** Compute the literal-node ID with the exact term codec in section 2, drain every page, and distinct-project `from` as `value`. VALUES joins on RDF term identity: a differently spelled literal with the same numeric value or timestamp is not an additional match. The source inequality is true for an RDF subject IRI/blank node versus this literal under the import contract. No numeric/temporal value evaluator is needed for this particular VALUES lookup.

**Dataset expectation:** `m.05vp56` (Kibibit per second). 1 recorded answer(s).

### 20. name the review of reflections on science

Index **6684**, qid **3201177013000**, level `i.i.d.`. Status: **J — forward two-hop candidates; native projection/comparison available**.

**Meaning.** Find reviews of the work reviewed by the supplied review. Preserve the two different predicates; no inverse relation is assumed from their names.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :book.review . 
?x1 :type.object.type :book.reviewed_work . 
VALUES ?x2 { :m.0bhcv_9 } 
?x1 :book.reviewed_work.reviews_of_this_work ?x0 . 
?x2 :book.review.work_reviewed ?x1 . 
FILTER ( ?x0 != ?x1 && ?x0 != ?x2 && ?x1 != ?x2  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='m.0bhcv_9')-[:book.review.work_reviewed]->(*) INCLUDE (to)-[:type.object.type]->(id='book.reviewed_work') JOIN (to)-[:book.reviewed_work.reviews_of_this_work]->(*) INCLUDE (join1.to)-[:type.object.type]->(id='book.review') WHERE to.id != 'm.0bhcv_9' AND join1.to.id != 'm.0bhcv_9' RETURN rows INCLUDE FIELDS edge.to,join1.to LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x2 = m.0bhcv_9`; `?x1 = edge.to`; `?x0 = join1.to`.

**Finish / missing support.** Evaluate `?x0 != ?x1` in the adapter, then project distinct `join1.to`. The source also excludes the supplied review itself: omitting that constant inequality can admit a self answer. Alternatively add `to.id != @join1.to.id`, select `RETURN DISTINCT join1.to.id AS value` without INCLUDE FIELDS and exhaust native continuation; a capacity/work error still needs an explicit client fallback.

**Dataset expectation:** `m.0bhcv_m` (In Short: An Urchin in the Storm). 1 recorded answer(s).

### 21. the chinese units of measurement incorpates how many units of length?

Index **628**, qid **2103336005000**, level `zero-shot`. Status: **P — exhaustive paged set count in the adapter**.

**Meaning.** Count distinct distance-unit entities linked from the Chinese measurement system. COUNT operates on the inner SELECT DISTINCT output.

**Source SPARQL:**

```sparql
SELECT (COUNT(?x0) AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :measurement_unit.distance_unit . 
VALUES ?x1 { :m.012tm2 } 
?x1 :measurement_unit.measurement_system.length_units ?x0 . 
FILTER ( ?x0 != ?x1  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='m.012tm2')-[:measurement_unit.measurement_system.length_units]->(*) INCLUDE (to)-[:type.object.type]->(id='measurement_unit.distance_unit') WHERE to.id != 'm.012tm2' RETURN edges INCLUDE FIELDS to LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x1 = m.012tm2`; `?x0 = to`; final scalar `value = cardinality(distinct ?x0)`.

**Finish / missing support.** Drain pages and count the distinct `to` values. Return one integer scalar, including zero for no matches. Legacy RETURN count remains bounded by LIMIT. Replace its projection with `RETURN COUNT(DISTINCT to.id) AS value` for an exact native count over the complete bounded input; this returns 0 on empty input or an explicit error on exhausted bounds.

**Dataset expectation:** `2`. 1 recorded answer(s).

### 22. what is the name of an organism with a height larger than 1.74?

Index **1107**, qid **2104544018000**, level `compositional`. Status: **T — typed comparison with class anchor and completeness handling**.

**Meaning.** Find organisms whose height satisfies the actual SPARQL condition `>= 1.74`, despite the question wording “larger than”.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :biology.organism . 
FILTER (?x1 >= "1.74"^^<http://www.w3.org/2001/XMLSchema#float>)
?x0 :biology.organism.height_meters ?x1 . 
FILTER ( ?x0 != ?x1  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='biology.organism')<-[:type.object.type]-(*) JOIN (from)-[:biology.organism.height_meters]->(*) RETURN rows INCLUDE FIELDS edge.from,join1.to LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x0 = edge.from`; `?x1 = join1.to` (height literal).

**Finish / missing support.** Hydrate each literal, compare using its datatype and SPARQL numeric promotion against the xsd:float threshold, check FILTERs, and distinct-project `edge.from`. `WHERE join1.to.props.numeric >= 1.74` is an optional fast path only after defining a compatible numeric import contract; float32 rounding and mixed datatypes can change boundary answers.

**Dataset expectation:** `m.0fr5sr` (Chandelier Tree); `m.09lzbv` (Bart the Bear); `m.0zdjzx0` (Euclid Tree); `m.04lj8n4` (Zenyatta); `m.01qtwv` (General Sherman); `m.0zdqswk` (Summit Road Tree); `m.0zdsn74` (Allen Russell); `m.0gw21k` (Hyperion). First 8 of 16 recorded answers shown.

### 23. which wine has vintage at 1986-01-01t00:00:00z?

Index **1249**, qid **2100967016000**, level `i.i.d.`. Status: **P — exact literal-term anchor plus paged result adaptation**.

**Meaning.** Find wines pointing to the exact xsd:dateTime term bound by VALUES. An equivalent instant written with another timezone spelling is a different term and is not selected by this query.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :wine.wine . 
VALUES ?x1 { "1986-01-01T00:00:00Z"^^<http://www.w3.org/2001/XMLSchema#dateTime> } 
?x0 :wine.wine.vintage ?x1 . 
FILTER ( ?x0 != ?x1  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='lit:bb91fe65bb6f0cf3598f347011012cff8a9c68de64079942135a3f1f10375b82')<-[:wine.wine.vintage]-(*) INCLUDE (from)-[:type.object.type]->(id='wine.wine') RETURN edges INCLUDE FIELDS from LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x0 = from`; `?x1 = lit:bb91fe65bb6f0cf3598f347011012cff8a9c68de64079942135a3f1f10375b82` represents `1986-01-01T00:00:00Z` with datatype `http://www.w3.org/2001/XMLSchema#dateTime`.

**Finish / missing support.** Compute the literal-node ID with the exact term codec in section 2, drain every page, and distinct-project `from` as `value`. VALUES joins on RDF term identity: a differently spelled literal with the same numeric value or timestamp is not an additional match. The source inequality is true for an RDF subject IRI/blank node versus this literal under the import contract. No numeric/temporal value evaluator is needed for this particular VALUES lookup.

**Dataset expectation:** `m.05n742f` (1986 De Loach Russian River Valley Late-Harvest Gewurztraminer); `m.07v_v0y` (1986 Château Pouget Margaux); `m.05n75hv` (1986 Pol Roger Sir Winston Churchill Cuvee Champagne); `m.0c6q9m8` (1986 Delaforce Colheita Tawny); `m.05mlcq3` (1986 Montdomaine Monticello Merlot); `m.0c63gx0` (1986 Warre's Quinta do Cavadinha Late Bottled Vintage); `m.07nwnnm` (1986 Haut Bailly). 7 recorded answer(s).

### 24. after the date of 05/08/2008, which song was added to rock band?

Index **913**, qid **4301069003000**, level `zero-shot`. Status: **T/J — mediator, temporal comparison, dynamic inequalities and completeness**.

**Meaning.** Find songs connected through a game-song relationship to the fixed game, where the relationship release date is at most the typed source date. The English question says “after”, but the query says `<=`.

**Source SPARQL:**

```sparql
SELECT (?x0 AS ?value) WHERE {
SELECT DISTINCT ?x0  WHERE { 
?x0 :type.object.type :cvg.musical_game_song . 
?x1 :type.object.type :cvg.musical_game_song_relationship . 
VALUES ?x2 { :m.0fq74z3 } 
FILTER (?x3 <= "2008-05-08-08:00"^^<http://www.w3.org/2001/XMLSchema#date>)
?x0 :cvg.musical_game_song.games ?x1 . 
?x1 :cvg.musical_game_song_relationship.game ?x2 . 
?x1 :cvg.musical_game_song_relationship.release_date ?x3 . 
FILTER ( ?x0 != ?x1 && ?x0 != ?x2 && ?x0 != ?x3 && ?x1 != ?x2 && ?x1 != ?x3 && ?x2 != ?x3  )
}
}
```

**Current Cheetah command:**

```text
GRAPH_QUERY MATCH (id='m.0fq74z3')<-[:cvg.musical_game_song_relationship.game]-(*) INCLUDE (from)-[:type.object.type]->(id='cvg.musical_game_song_relationship') JOIN (from)<-[:cvg.musical_game_song.games]-(*) INCLUDE (join1.from)-[:type.object.type]->(id='cvg.musical_game_song') JOIN (from)-[:cvg.musical_game_song_relationship.release_date]->(*) WHERE from.id != 'm.0fq74z3' AND join1.from.id != 'm.0fq74z3' RETURN rows INCLUDE FIELDS edge.from,join1.from,join2.to LIMIT 2048 BUDGET 1048576
```

**Bindings.** `?x2 = m.0fq74z3`; `?x1 = edge.from` (relationship mediator); `?x0 = join1.from` (song); `?x3 = join2.to` (date literal).

**Finish / missing support.** Hydrate `join2.to`; evaluate `<= "2008-05-08-08:00"^^xsd:date` with its timezone, then all pairwise FILTERs. Project distinct songs. Keep the date attached to the relationship; flattening it onto a song would mix releases for different games. Bytewise string ordering is not a general date comparison. The source records 137 answers, which already exceeds Cheetah's default LIMIT 128.

**Dataset expectation:** `m.048cbj` (More Than a Feeling); `m.04csrvd` (Hard to Handle); `m.010x7g2` (Gimme Three Steps); `m.04_czx` (Won't Get Fooled Again); `m.0lfg_q` (N.I.B.); `m.0bb4dch` (Blackened); `m.0y6lj4` (March of the Pigs); `m.026dtxk` (Here It Goes Again). First 8 of 137 recorded answers shown.

## 5. Compiler design for a first correct converter

A first version can use the existing server and place missing relational/RDF semantics in the
client. Native extensions can then replace expensive client operators without changing the logical
plan or expected answers. Keep the converter separate from the trie; namespace-specific Freebase
logic does not belong in the storage core.

### 5.1 Parse and normalize

Use a real SPARQL parser with source locations, not regex substitutions. Expand PREFIX declarations;
represent IRIs, variables, literals and blank nodes as different term kinds. Preserve nested SELECT
scopes, DISTINCT, VALUES, FILTER, aggregate expressions and aliases. The outer `?value` projection
and COUNT wrapper are meaningful. Only flatten nested SELECTs after proving their scope and
multiplicity behavior is preserved.

Build a logical intermediate representation along these lines (operator names are compiler concepts,
**not new Cheetah syntax**):

```text
Term = IRI | Literal(lexical, datatype, language) | BlankNode | Variable(scope, name)
Plan = ScanTriple | Values | Join | Filter | Project | Distinct | Aggregate | Slice
BindingMap = SPARQL variable -> one or more Cheetah endpoint/literal references
```

Detect features outside the supported subset and return a diagnostic such as
`unsupported_sparql_feature` with its source span. Do not turn OPTIONAL, UNION, variable predicates,
property paths, language functions or correlated subqueries into approximate recall if encountered
in later datasets. They need explicitly implemented algebra first. Query function metadata is useful
for sample reporting, but parsing the actual SPARQL determines execution.

### 5.2 Plan from bound IDs

1. Fold singleton VALUES terms into exact term constants, keeping FILTERs that constrain them. Multiple VALUES
   rows need explicit enumeration/join semantics; they are not one comma-separated node ID.
2. Represent each triple as a directed constraint, retaining all occurrences of a shared variable.
   Choose a constant entity anchor where selective; otherwise use an explicit class membership edge
   in reverse. A class constant is a legal anchored node, not a forbidden label-only graph scan.
3. Order joins so each next edge has a bound subject or object. Map outgoing/incoming patterns to
   the same authoritative edge orientation (`from = RDF subject`, `to = RDF object`).
4. Use INCLUDE only for a relation needed solely for existence, when this preserves the surrounding
   bag/set semantics. All present examples have an inner SELECT DISTINCT answer, allowing several
   useful semijoin reductions. Do not erase a bound variable needed by another triple, FILTER,
   aggregate or projection.
5. Push safe constant predicates into WHERE and term-ID comparisons into explicit `@field`
   comparisons. Keep incompatible null/error behavior and RDF datatype work in client operators.
   Bare field-looking RHS text is still a literal; the `@` is mandatory for a bound reference.
6. Preserve each mediator. Check the eight-relation/32-field limits before emitting a native query;
   split the execution plan rather than dropping constraints to fit the grammar.

For example, case 05 has this binding map after reordering around the constant:

| Variable | Role | Cheetah row location |
| --- | --- | --- |
| `?x2` | millisiemens constant | `m.0gvb8ml` |
| `?x1` | shared measurement system | `edge.to` |
| `?x0` | answer frequency unit | `join1.from` |

In an incoming match, the syntactic left anchor may be the edge's `to` endpoint. Referring to the
left position as `from` would silently reverse the answer variable. For rows projection, JSON paths
are `edge.from`, `join1.to`, etc.; WHERE uses `from.id`, `join1.to.id`, etc. Those are different
namespaces and must be rendered separately.

### 5.3 Execute completely with the current server

For a simple paged command:

```text
bindings = empty set or a bounded/spillable distinct structure
cursor = no cursor
repeat:
    send the command, adding CURSOR only when resuming
    require SUCCESS; decode payload from base64 JSON
    turn each row into a binding and apply remaining filters
    retain distinct answer terms, or stream them to a bounded/spillable operator
    cursor = next_cursor
until cursor == '*'
```

For a join that may exceed native output limits, use an exhaustive client nested-loop plan:

```text
expand(binding, remaining triple constraints):
    if none remain: evaluate FILTERs and emit the binding
    else:
        choose a constraint with a bound endpoint, including class constants
        substitute that endpoint as MATCH's concrete left ID
        enumerate every page of matching edges with a single-hop command
        unify subject/object with the existing binding (check repeated variables)
        expand(each compatible extended binding, remaining constraints)
```

Concretely, case 05 can page millisiemens-to-system edges first, verify the system's type, then for
each system page incoming frequency-unit edges and check their type. It can then apply all three
inequalities and distinct-project units. This costs more round trips but retains all bindings and
can finish without an unpageable native JOIN. Type/constant existence probes can use INCLUDE or
small point queries. Exact VALUES literals can anchor directly on the term-node ID. For typed
FILTERs with no fixed term, start from the class node and page the literal predicate per member.

Native JOIN now preserves its complete nested adjacency path in CURSOR and returns explicit
`complete=true|false`. Page until complete; LIMIT bounds each page rather than the entire joined
answer. Do not reuse these tokens for a different query, after graph mutation, or after reopen.
LIMIT/BUDGET/CACHE can change between pages. Exhausted work/output/cursor capacity is an error,
not a successful incomplete page. Client expansion remains a fallback for work/state bounds.
`RETURN DISTINCT ... AS value` exhausts the bounded logical input and deduplicates globally before
paging; each page recomputes the full set. Exact aggregate expressions also require complete input.
Apply every residual RDF predicate before DISTINCT/count/extrema, keeping those stages in the
adapter whenever a native scalar predicate cannot faithfully express the RDF operation.

Bound converter memory, requests, intermediate bindings and elapsed time; cooperate with server
back-off and error responses. Spill distinct/intermediate bindings to a client-owned temporary store
or stop with an explicit incomplete/error result. Never turn a resource stop into COUNT 0, an empty
answer set, or a partial MIN/MAX. Deduplication for the final SELECT DISTINCT is global across all
pages and all expansion branches, not local to a page. Keep a quiescent imported graph for benchmark
runs: the current command sequence does not provide a cross-request RDF snapshot.

### 5.4 Evaluate semantics and shape the answer

- Evaluate VALUES compatibility, repeated-variable unification and original FILTERs with RDF-aware
  rules. FILTER errors reject the affected solution; they are not arbitrary true/false defaults.
  Retain the pairwise inequalities generated by GrailQA; distinct variable names do not mean
  distinct nodes. A kind proof may eliminate an IRI-versus-literal inequality only if valid under
  the chosen SPARQL datatype/error rules.
- Keep term identity distinct from numeric/temporal value equality. Use RDF-aware numeric promotion,
  timezone handling and comparison errors; do not rely on Cheetah's missing-property `!=` behavior.
- For entity answers, emit the IDs bound to the outer answer variable, never all endpoint IDs.
  Resolve entity names afterward for display only. Produce the outer alias `value` in the adapter.
- Implement COUNT on the actual subquery output. In these count templates the inner DISTINCT comes
  before the outer count; a joined-row count or per-page count can overcount or truncate.
- For MIN/MAX subqueries, exhaust the exact inner relation with its constraints, compute the scalar
  using SPARQL aggregate ordering, and join that RDF term back to the outer relation. Retain every tie
  and apply outer DISTINCT afterward. The inner and outer variable scopes must remain separate.
  On an empty aggregate input, follow SPARQL's aggregate/binding behavior rather than inventing a
  winner, a date or a zero cost. In these queries the join-back uses term identity, not a new numeric
  FILTER: equal-valued lexical variants are not automatically all winners. If ordering leaves ties
  between distinct terms undefined, document the reference engine's behavior rather than claiming a
  unique cross-engine result. See the definitions of
  [MIN/MAX](https://www.w3.org/TR/sparql11-query/#defn_aggMin) and
  [join compatibility](https://www.w3.org/TR/sparql11-query/#defn_algCompatibleMapping).
- Validate cardinality and values, not incidental row order: SELECT DISTINCT without ORDER BY does
  not promise the adjacency ordering. Compare entity result sets by ID and scalar results by a
  declared typed-result normalization.

### 5.5 Translation output contract

The later program should emit a reviewable plan, not just an opaque string:

```json
{
  "qid": "2102557006000",
  "source_index": 447,
  "source_field": "sparql_query",
  "import_contract_version": "freebase-rdf-v1",
  "execution_mode": "paged_native_with_adapter",
  "commands": ["GRAPH_QUERY MATCH ..."],
  "bindings": {"?x0": "from", "?x1": {"iri": "m.0vs7y5v"}},
  "postprocess": ["project ?x0 AS value", "distinct across all pages"],
  "completion_rule": "all page cursors exhausted without errors",
  "required_features": [],
  "warnings": []
}
```

This is an illustrative converter output schema, not a server command or an implemented tool.
For typed/joined/aggregate cases, record the residual predicates, literal hydrations, execution
operator tree, resource limits, fallback strategy and required features explicitly. Keep deterministic
sampling manifests and the dataset hash alongside generated plans for regression comparisons.

## 6. Native support implemented and remaining priorities

The generic query-language extensions below now use the existing GRAPH_QUERY planner/executor
and shared command dispatch. RDF import, typed evaluation and a SPARQL compiler remain future work
tracked in [NEXT_STEPS.md](../NEXT_STEPS.md). No new on-disk byte format or SPARQL TCP surface was added.

| Priority | Capability | Cases motivating it | Concrete acceptance condition / likely owner |
| --- | --- | --- | --- |
| P0 — shipped | Explicit joined completion and resumable execution | All J/T/A candidate commands | Full nested adjacency cursors, `complete=true|false`, one matching-row lookahead; authenticated per open database handle and guarded by graph epoch. `graph_query_cursor.go`, `graph_query_bindings.go` |
| P0 — shipped | Bound field-to-field comparisons / variable equality | 05, 14, 16, 20, 24 | Explicit `@field` comparisons resolve bound scalars; unknown joins are rejected; bare RHS literals remain unchanged. JSON null/missing comparisons are false. `graph_query_expr.go` |
| P1 — shipped | Answer-variable projection, aliases and global DISTINCT | Every example | `RETURN [DISTINCT] field AS value[,...]`; full-input deduplication before slicing, 65,536 keys / 8 MiB shared state, explicit capacity errors. `graph_query_select.go`, `graph_query_bindings.go` |
| P1 — generic operators shipped; RDF pending | Arbitrary-field aggregates and exact count | 08, 16, 21 | Exact `COUNT(*)`, `COUNT([DISTINCT] field)`, `MIN(field)`, `MAX(field)` with aliases and complete input. Empty COUNT is 0, MIN/MAX null. Uniform JSON number/string extrema only; typed RDF aggregate ordering and term-identity join-back remain client work. Legacy GROUP BY is unchanged |
| P1 | Versioned RDF term import and typed comparison | 07, 08, 16, 19, 22–24 | Preserve term identity for VALUES (07/19/23); provide numeric/temporal evaluation for FILTER/aggregate cases (08/16/22/24), with datatype errors and timezone handling. Start in the converter; add generic engine capabilities only after semantics are pinned |
| P2 | Selective value/range access | 22, 24 and larger aggregate inputs | Derived, verified predicate/value indexes reduce class-member enumeration without changing answers or loading graph-wide candidate maps. Exact literal VALUES lookups already have a literal-node adjacency anchor; a new range/index planner is a separate design |
| P2 | Import readiness, consistency and plan inspection | Whole evaluation | Report import contract/version and graph snapshot identity; expose why an anchor/order was chosen and which operators remain in the client. Add native snapshot support only if concurrent evaluation requires it |

Arbitrary unanchored scans are not the first missing feature to add: the present class-node anchor
already handles the sampled no-entity queries without abandoning indexed adjacency. ORDER BY alone
also does not fix extrema: ORDER BY ... LIMIT 1 loses tied winners, and it needs complete typed
input anyway. Freebase CVTs can already be stored; the missing part is a faithful binding/planning
layer, not a new storage format for the sampled mediators.

For each engine extension, preserve one-line responses and the single dispatch shared by CLI/TCP
and binary transport; update both binders, README and AGENTS.md in the same implementation change.
Use the existing telemetry/back-off, managed IO, cache epoch and capacity mechanisms. Derived RDF
indexes must be rebuildable and verify authoritative graph records. No on-disk byte-width change is
required merely to express the sampled query shapes.

### Native forms for the converter

For a simple answer variable, retain the documented MATCH/INCLUDE/WHERE and replace
`RETURN edges INCLUDE FIELDS from` with `RETURN DISTINCT from.id AS value` (or `to.id` for
an outgoing answer). This yields `[{"value":"entity-id"},...]`; page until complete. If the full
distinct input exceeds bounds, retain the original paged command and use a spillable client distinct.

Case 05 can now express its dynamic inequality and answer projection together:

```text
GRAPH_QUERY MATCH (id='m.0gvb8ml')-[:measurement_unit.conductance_unit.measurement_system]->(*) INCLUDE (to)-[:type.object.type]->(id='measurement_unit.measurement_system') JOIN (to)<-[:measurement_unit.unit_of_frequency.measurement_system]-(*) INCLUDE (join1.from)-[:type.object.type]->(id='measurement_unit.unit_of_frequency') WHERE to.id != 'm.0gvb8ml' AND join1.from.id != 'm.0gvb8ml' AND to.id != @join1.from.id RETURN DISTINCT join1.from.id AS value LIMIT 2048 BUDGET 1048576
```

Case 21 can compute the exact distinct count directly:

```text
GRAPH_QUERY MATCH (id='m.012tm2')-[:measurement_unit.measurement_system.length_units]->(*) INCLUDE (to)-[:type.object.type]->(id='measurement_unit.distance_unit') WHERE to.id != 'm.012tm2' RETURN COUNT(DISTINCT to.id) AS value BUDGET 1048576
```

`RETURN COUNT(*) AS rows,MIN(join1.to.props.numeric) AS low,MAX(join1.to.props.numeric) AS high`
illustrates multiple arbitrary-field aggregates **only for an explicitly compatible numeric import**.
It does not make `props.numeric` authoritative RDF data. Do not substitute bytewise MIN/MAX of
`lexical` for cases 08/16, or float64 comparisons for cases 22/24 without datatype validation.
Aliases use identifiers, at most 32 selected fields are accepted, and aggregate expressions cannot
be mixed with ordinary fields or GROUP BY. Joined legacy return modes and all new selected modes
expose completion; existing single-hop legacy wire shapes remain unchanged.

## 7. Validation performed and later conformance suite

- Verified all 24 displayed source bodies against the selected JSON rows, the dataset hash, qids,
  selection order, command manifest, code fences and local documentation links.
- Parsed and executed all 24 documented commands on synthetic graphs with both `pair_index_bytes=1`
  and `pair_index_bytes=2` (48 command/stride cases). Each fixture had a matching triple neighborhood
  and a distractor; returned projected endpoint IDs were checked against the documented variable
  bindings. The temporary Go validation harness was removed after running.
- The native extension regressions in [`graph_query_conversion_test.go`](../src/graph_query_conversion_test.go)
  verify 2,340 paged chained/reverse join bindings on both trie strides, bound comparisons,
  answer aliases, global DISTINCT, complete-input count/extrema, empty/tied/missing/mixed-value
  cases, distinct capacity errors and stale/forged/query-changed/foreign/reopened-handle tokens.
  Text/binary parity is checked in [`graph_query_test.go`](../src/graph_query_test.go); both binder
  unit suites and live text/binary projection/continuation/count tests passed. Focused `-race`
  checks passed for these query paths.
- `go build ./...`, `go vet ./...` and `go test ./src` passed. The normal source suite includes query
  parser, joins, filters, projection, grouping and pagination coverage.

These checks establish that the commands use current grammar and traverse/project the intended
small graph shapes. The future adapter's RDF FILTER/aggregate processing and complete Freebase
answers were not executed: no converter or full RDF import was created for this study.

A later converter should keep all 24 qids as a regression manifest and add fixtures that intentionally
expose the differences a happy-path example can hide:

| Fixture | Required outcome |
| --- | --- |
| Wrong class, missing intermediate class, self edge | Respect every type constraint and original inequality |
| Incoming edges and mixed-direction joins | Resolve the answer variable by edge orientation |
| Same answer reached by several paths/pages | One result after global DISTINCT; correct COUNT |
| 129 and >2,048 candidate rows | Default/max LIMIT cannot silently turn into a complete answer; all pages/continuations agree |
| Numeric lexical variants, float threshold boundaries, integer/float mixtures | Match the reference RDF evaluator, not string or approximate-number heuristics |
| date/dateTime with equivalent offsets, absent timezone, gYear/gYearMonth | Typed equality/order and error handling preserved |
| Missing, invalid, incomparable literal values | Correct FILTER/aggregate behavior, with no invented zero or timestamp |
| Two equal maxima/minima, one subject with multiple values | Preserve every tied winning entity and distinct afterward |
| Same song with releases in different games | Apply the release date to the correct mediator binding |
| Exhausted budget/group/output capacity, interrupted import or changed graph | Explicit failure/incomplete status, never a fabricated exact answer |

On a complete, fixed Freebase import, compare translated results against a reference SPARQL engine
using the same RDF snapshot and datatype policy; then compare to GrailQA's recorded answers. A
mismatch to the supplied answer JSON can also come from a graph-version mismatch, so record both
comparisons. Run the converter corpus on both trie strides. Measure commands issued, edges examined,
node hydrations, intermediate cardinalities, bytes and latency separately from semantic accuracy.
Random sample coverage and synthetic fixtures are useful gates, not evidence of full GrailQA support.
