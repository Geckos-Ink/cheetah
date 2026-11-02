package main

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestGraphQueryExpressions(t *testing.T) {
	for _, stride := range []int{1, 2} {
		t.Run(fmt.Sprint(stride), func(t *testing.T) {
			db := newAdaptiveTestDB(t, stride, true, 4096)
			props := base64.StdEncoding.EncodeToString([]byte(`{"Rank":7,"title":"O'Reilly  WHERE  JOIN","active":true,"empty":null}`))
			assertCommandPrefix(t, db, "GRAPH_NODE_SET id=b labels=person props="+props, "SUCCESS")
			assertCommandPrefix(t, db, "GRAPH_EDGE_SET from=a to=b type=link weight=0.75 props="+props, "SUCCESS")
			for _, tc := range []struct {
				where string
				want  int
			}{
				{"edge.weight != 0.5", 1}, {"edge.weight <> 0.75", 0},
				{"edge.weight < 1 AND edge.weight > 0.5", 1},
				{"edge.weight <= 0.75 AND edge.weight >= 0.75", 1},
				{"to.id > 'a' AND edge.type < 'z' AND edge.props.Rank >= 7", 1},
				{"to.props.Rank = 7 AND to.label IN ('company','person')", 1},
				{"to.label NOT IN ('person')", 0},
				{"edge.weight = 0 OR edge.weight = 0.75 AND to.id = 'b'", 1},
				{"(edge.weight = 0 OR edge.weight = 0.75) AND NOT to.id = 'b'", 0},
				{"NOT (edge.weight = 0 OR to.id != 'b')", 1},
				{"edge.props.active IN (false,true) AND edge.type NOT IN ('other','wrong')", 1},
				{"edge.props.Rank IN (1,7,9)", 1},
				{"edge.props.empty IS NULL AND to.props.missing IS NULL AND to.props.Rank IS NOT NULL", 1},
				{"edge.props.missing != 'x'", 1},
				{"edge.props.title = 'O''Reilly  WHERE  JOIN'", 1},
				{`edge.props.title = 'O\'Reilly  WHERE  JOIN'`, 1},
				{`edge.props.title = 'O\u0027Reilly  WHERE  JOIN'`, 1},
				{"edge.props.title >= 'O' AND edge.props.title < 'P'", 1},
				{"edge.directed = true AND edge.confidence >= possible AND edge.modality >= probable", 1},
			} {
				response := assertCommandPrefix(t, db, "GRAPH_QUERY MATCH (id='a')-[:link]->(*) WHERE "+tc.where+" RETURN count", "SUCCESS")
				if responseField(response, "matches") != fmt.Sprint(tc.want) {
					t.Errorf("WHERE %s: %s", tc.where, response)
				}
			}
		})
	}
}

func TestGraphQueryRelationalParserRejectsUnboundedOrMalformedPlans(t *testing.T) {
	base := "MATCH (id='a')-[:link]->(*) "
	for _, tail := range []string{
		"WHERE edge.weight = NaN", "WHERE edge.weight = Inf", "WHERE edge.props.x = null",
		"WHERE edge.weight => 2", "WHERE edge.type = 'unterminated", "WHERE (edge.weight = 1",
		"WHERE edge.weight = 1 AND", "WHERE to.label > 'a'", "WHERE edge.directed > true",
		"WHERE edge.type IN ()", "WHERE edge.weight = 1 WHERE edge.weight = 2",
		"WHERE join1.to.id = 'x'", "JOIN (*)-[:t]->(*)", "JOIN (id='x')-[:t]->(*)",
		"JOIN (join1.to)-[:t]->(*)", "JOIN (to)-[:t]->(*) CURSOR x01",
		"JOIN (to)-[:t]->(*) HOPS 2", "GROUP BY edge.type CURSOR x01",
		"HOPS 17..18", "BRANCH 2 BRANCH_LIMIT 3", "COST 1 COST_LIMIT 2",
		"GROUP BY edge.type RETURN edges", "RETURN groups", "RETURN count INCLUDE FIELDS id",
		"INCLUDE (to)-[:t]->(*) HOPS 2", "BUDGET 0", "BUDGET 1048577", "CACHE serve",
		"INCLUDE FIELDS a..b", "LIMIT 2 LIMIT 3", "BOGUS 1",
		"WHERE " + strings.Repeat("NOT ", 33) + "edge.weight = 1",
	} {
		if _, err := parseGraphQuery(base + tail); err == nil {
			t.Errorf("accepted invalid query: %s", tail)
		}
	}
	if _, err := parseGraphQuery("MATCH (label='person')-[:t]->(*)"); err == nil {
		t.Fatal("label-only anchor allowed a global scan")
	}
}

func TestGraphQueryMultihopFrontierIsBounded(t *testing.T) {
	db := newAdaptiveTestDB(t, 1, true, 4096)
	for depth := 0; depth < 14; depth++ {
		for from := 0; from < 2; from++ {
			for to := 0; to < 2; to++ {
				assertCommandPrefix(t, db, fmt.Sprintf("GRAPH_EDGE_SET from=d%d_%d to=d%d_%d type=link", depth, from, depth+1, to), "SUCCESS")
			}
		}
	}
	response, err := db.ExecuteCommand("GRAPH_QUERY MATCH (id='d0_0')-[:link]->(id='absent') HOPS 14 BRANCH_LIMIT 2 BUDGET 1048576 RETURN count")
	if err == nil || err.Error() != "graph_query_frontier_exceeded" || strings.Contains(response, "SUCCESS") {
		t.Fatalf("frontier was not bounded: %s %v", response, err)
	}
}

func TestGraphQueryVerifiesAdjacencyCandidates(t *testing.T) {
	db := newAdaptiveTestDB(t, 2, true, 4096)
	assertCommandPrefix(t, db, "GRAPH_EDGE_SET from=a to=b type=link", "SUCCESS")
	assertCommandPrefix(t, db, "GRAPH_EDGE_SET from=x to=y type=link", "SUCCESS")
	// Simula un puntatore riciclato prima che l'indice derivato venga ripulito.
	if _, err := db.upsertPairPayload(graphAdjOutPairKey("a", "link", "b", true), graphEdgePairKey("x", "y", "link", true), true); err != nil {
		t.Fatal(err)
	}
	response := assertCommandPrefix(t, db, "GRAPH_QUERY MATCH (id='a')-[:link]->(*) RETURN count", "SUCCESS")
	if responseField(response, "matches") != "0" {
		t.Fatalf("stale adjacency escaped its bound endpoint: %s", response)
	}
}

func TestGraphQueryJoinsAndRelationshipFilters(t *testing.T) {
	for _, stride := range []int{1, 2} {
		t.Run(fmt.Sprint(stride), func(t *testing.T) {
			db := newAdaptiveTestDB(t, stride, true, 4096)
			for _, id := range []string{"acme", "other"} {
				assertCommandPrefix(t, db, "GRAPH_NODE_SET id="+id+" labels=company", "SUCCESS")
			}
			for _, edge := range []string{
				"from=hub to=u1 type=knows weight=0.5", "from=hub to=u2 type=knows weight=1", "from=hub to=u3 type=knows weight=0.25",
				"from=u1 to=acme type=works_at", "from=u1 to=other type=works_at", "from=u2 to=acme type=works_at",
				"from=acme to=rome type=located_in", "from=other to=milan type=located_in", "from=u2 to=hub type=blocked",
				"from=acme to=u1 type=employs", "from=acme to=u2 type=employs",
			} {
				assertCommandPrefix(t, db, "GRAPH_EDGE_SET "+edge, "SUCCESS")
			}
			base := "GRAPH_QUERY MATCH (id='hub')-[:knows]->(*) "
			for _, tc := range []struct {
				clauses string
				count   string
			}{
				{"INCLUDE (to)-[:works_at]->(id='acme') RETURN count", "2"},
				{"EXCLUDE (to)-[:works_at]->(id='acme') RETURN count", "1"},
				{"INCLUDE (to)-[:works_at]->(id='acme') EXCLUDE (to)-[:blocked]->(id='hub') RETURN count", "1"},
				{"INCLUDE (to)<-[:employs]-(id='acme') RETURN count", "2"},
				{"JOIN (to)-[:works_at]->(label='company') RETURN count", "3"},
				{"JOIN (to)-[:works_at]->(*) JOIN (join1.to)-[:located_in]->(*) WHERE join2.to.id = 'rome' RETURN count", "2"},
				{"JOIN (to)-[:works_at]->(*) INCLUDE (join1.to)-[:located_in]->(id='rome') RETURN count", "2"},
				{"JOIN (to)-[:works_at]->(*) WHERE edge.weight = 0 OR join1.to.id = 'other' RETURN count", "1"},
				{"WHERE edge.weight = 0 OR edge.weight = 0.5 JOIN (to)-[:works_at]->(*) RETURN count", "2"},
			} {
				response := assertCommandPrefix(t, db, base+tc.clauses, "SUCCESS")
				if responseField(response, "matches") != tc.count {
					t.Errorf("%s: %s", tc.clauses, response)
				}
			}
			response := assertCommandPrefix(t, db, base+"JOIN (to)-[:works_at]->(*) RETURN rows INCLUDE FIELDS edge.to,join1.to EXCLUDE FIELDS edge.id", "SUCCESS")
			var rows []map[string]map[string]interface{}
			decodePayloadField(t, response, &rows)
			if len(rows) != 3 || len(rows[0]["edge"]) != 1 || rows[0]["join1"]["to"] == nil {
				t.Fatalf("unexpected joined projection: %+v", rows)
			}
			response = assertCommandPrefix(t, db, "GRAPH_QUERY MATCH (id='u1')<-[:knows]-(*) JOIN (from)-[:knows]->(id='u2') RETURN count", "SUCCESS")
			if responseField(response, "matches") != "1" {
				t.Fatal(response)
			}
		})
	}
}

func TestGraphQueryGroupingStreamsAllRows(t *testing.T) {
	for _, stride := range []int{1, 2} {
		t.Run(fmt.Sprint(stride), func(t *testing.T) {
			db := newAdaptiveTestDB(t, stride, true, 4096)
			for i := 0; i < 270; i++ {
				assertCommandPrefix(t, db, fmt.Sprintf("GRAPH_EDGE_SET from=hub to=n%03d type=t%d weight=%d", i, i%2, 1+i%2), "SUCCESS")
			}
			response := assertCommandPrefix(t, db, "GRAPH_QUERY MATCH (id='hub')-[:*]->(*) GROUP BY edge.type RETURN groups LIMIT 1", "SUCCESS")
			var groups []graphQueryGroup
			decodePayloadField(t, response, &groups)
			if len(groups) != 1 || groups[0].Count != 135 || groups[0].WeightSum != 135 || groups[0].WeightAvg != 1 || groups[0].WeightMin != 1 || groups[0].WeightMax != 1 {
				t.Fatalf("grouping stopped at LIMIT or an internal page: %+v", groups)
			}
			response = assertCommandPrefix(t, db, "GRAPH_QUERY MATCH (id='hub')-[:*]->(*) WHERE edge.type = 't1' GROUP BY edge.type", "SUCCESS")
			decodePayloadField(t, response, &groups)
			if len(groups) != 1 || groups[0].Count != 135 || groups[0].WeightSum != 270 || groups[0].WeightAvg != 2 {
				t.Fatalf("wrong filtered group: %+v", groups)
			}
			for _, tail := range []string{"GROUP BY edge.type", "WHERE edge.weight = 99 RETURN count"} {
				response, err := db.ExecuteCommand("GRAPH_QUERY MATCH (id='hub')-[:*]->(*) " + tail + " BUDGET 10")
				if err == nil || err.Error() != "graph_query_budget_exceeded" || strings.Contains(response, "SUCCESS") {
					t.Fatalf("budget exhaustion returned a partial answer: %s %v", response, err)
				}
			}
		})
	}
}

func TestGraphQueryProjectionPreservesPayloadsAndPagination(t *testing.T) {
	for _, stride := range []int{1, 2} {
		t.Run(fmt.Sprint(stride), func(t *testing.T) {
			db := newAdaptiveTestDB(t, stride, true, 4096)
			props := base64.StdEncoding.EncodeToString([]byte(`{"name":"visible","private":{"secret":"hidden","public":true}}`))
			for i := 0; i < 7; i++ {
				assertCommandPrefix(t, db, fmt.Sprintf("GRAPH_EDGE_SET from=hub to=n%d type=link props=%s", i, props), "SUCCESS")
				if i%2 == 0 {
					assertCommandPrefix(t, db, fmt.Sprintf("GRAPH_EDGE_SET from=n%d to=ok type=allowed", i), "SUCCESS")
				}
			}
			cursor, seen := "*", map[string]bool{}
			for page := 0; page < 10; page++ {
				cmd := "GRAPH_QUERY MATCH (id='hub')-[:link]->(*) INCLUDE (to)-[:allowed]->(id='ok') RETURN edges INCLUDE FIELDS to,props EXCLUDE FIELDS props.private.secret LIMIT 1 CURSOR " + cursor
				response := assertCommandPrefix(t, db, cmd, "SUCCESS")
				var rows []map[string]interface{}
				decodePayloadField(t, response, &rows)
				for _, row := range rows {
					id := row["to"].(string)
					if seen[id] || len(row) != 2 {
						t.Fatalf("bad projection/page: %+v", row)
					}
					seen[id] = true
					private := row["props"].(map[string]interface{})["private"].(map[string]interface{})
					if len(private) != 1 || private["public"] != true {
						t.Fatalf("wrong nested exclusion: %+v", private)
					}
				}
				cursor = responseField(response, "next_cursor")
				if cursor == "*" {
					break
				}
			}
			if len(seen) != 4 || cursor != "*" {
				t.Fatalf("filtered pages lost rows: %v cursor=%s", seen, cursor)
			}
			edge, found, err := db.graphGetEdge("hub", "n0", "link", true)
			if err != nil || !found || edge.Props["private"].(map[string]interface{})["secret"] != "hidden" {
				t.Fatal("projection mutated the authoritative payload")
			}
		})
	}
}

func TestGraphQueryAssociationCacheAdmissionInvalidationAndAging(t *testing.T) {
	db := newAdaptiveTestDB(t, 2, true, 4096)
	store := forceGraphCache(t, db, true)
	store.setConfig(defaultGraphCacheConfig())
	now := time.Now()
	store.setNow(func() time.Time { return now })
	assertCommandPrefix(t, db, "GRAPH_EDGE_SET from=hub to=person type=knows", "SUCCESS")
	for i := 0; i < 12; i++ {
		assertCommandPrefix(t, db, fmt.Sprintf("GRAPH_EDGE_SET from=person to=n%d type=works_at", i), "SUCCESS")
	}
	query := "GRAPH_QUERY MATCH (id='hub')-[:knows]->(*) EXCLUDE (to)-[:works_at]->(label='banned') RETURN count"
	assertCommandPrefix(t, db, query, "SUCCESS,return=count,matches=1")
	epoch := store.currentEpoch()
	if store.entries.Load() != 0 {
		t.Fatal("first query created a cache row")
	}
	assertCommandPrefix(t, db, query, "SUCCESS,return=count,matches=1")
	if store.entries.Load() != 1 {
		t.Fatal("repeated expensive association was not admitted")
	}
	if store.currentEpoch() != epoch {
		t.Fatal("SQL cache admission invalidated the graph epoch")
	}
	visible, _, err := db.PairScanWithOptions([]byte(graphCacheQueryPrefix), 8, nil, false)
	if err != nil || len(visible) != 0 {
		t.Fatalf("SQL cache rows leaked into ordinary scans: %d %v", len(visible), err)
	}
	before := store.metrics.Hits.Load()
	assertCommandPrefix(t, db, query+" CACHE off", "SUCCESS,return=count,matches=1")
	if store.metrics.Hits.Load() != before {
		t.Fatal("CACHE off read the association memo")
	}
	assertCommandPrefix(t, db, query, "SUCCESS,return=count,matches=1")
	if store.metrics.Hits.Load() <= before {
		t.Fatal("repeated association did not use cache")
	}
	assertCommandPrefix(t, db, "GRAPH_NODE_SET id=n0 labels=banned", "SUCCESS")
	assertCommandPrefix(t, db, query, "SUCCESS,return=count,matches=0")
	if store.metrics.Stale.Load() == 0 {
		t.Fatal("node mutation did not invalidate a negative association")
	}
	rows, _, err := db.PairScanWithOptions([]byte(graphCacheQueryPrefix), 8, nil, true)
	if err != nil || len(rows) != 1 {
		t.Fatalf("cache rows: %d %v", len(rows), err)
	}
	if rows[0].Key == 0 {
		t.Fatal("cache row has no payload")
	}
	store.setNow(func() time.Time { return now.Add(7 * 24 * time.Hour) })
	for page := 0; page < 8; page++ {
		result, err := store.sweep(256, true)
		if err != nil {
			t.Fatal(err)
		}
		if result.Wrapped {
			break
		}
	}
	if store.entries.Load() != 0 {
		t.Fatal("unused SQL association did not decay away")
	}
}

func TestGraphQueryAssociationCacheNeverStoresIncompleteScans(t *testing.T) {
	db := newAdaptiveTestDB(t, 1, true, 4096)
	store := forceGraphCache(t, db, true)
	store.setConfig(defaultGraphCacheConfig())
	assertCommandPrefix(t, db, "GRAPH_EDGE_SET from=hub to=person type=knows", "SUCCESS")
	for i := 0; i < 12; i++ {
		assertCommandPrefix(t, db, fmt.Sprintf("GRAPH_EDGE_SET from=person to=n%d type=works_at", i), "SUCCESS")
	}
	for i := 0; i < 3; i++ {
		_, err := db.ExecuteCommand("GRAPH_QUERY MATCH (id='hub')-[:knows]->(*) EXCLUDE (to)-[:works_at]->(label='banned') BUDGET 5 RETURN count")
		if err == nil || store.entries.Load() != 0 {
			t.Fatal("an exhausted existence check published a negative memo")
		}
	}
}

func TestGraphQueryTextAndBinaryShareRelationalDispatch(t *testing.T) {
	engine, db := newRecordTestEngine(t)
	assertCommandPrefix(t, db, "GRAPH_EDGE_SET from=hub to=person type=knows weight=0.5", "SUCCESS")
	assertCommandPrefix(t, db, "GRAPH_EDGE_SET from=person to=org type=works_at", "SUCCESS")
	clause := "MATCH (id='hub')-[:knows]->(*) JOIN (to)-[:works_at]->(*) WHERE edge.weight <> 0 AND NOT join1.to.id = 'closed' GROUP BY join1.to.id RETURN groups"
	want := assertCommandPrefix(t, db, "GRAPH_QUERY "+clause, "SUCCESS")
	for _, binary := range []bool{false, true} {
		t.Run(fmt.Sprint(binary), func(t *testing.T) {
			client, peer := net.Pipe()
			server := NewTCPServer("127.0.0.1:0", engine, 0)
			done := make(chan struct{})
			go func() { server.handleConnection(peer); close(done) }()
			defer func() { client.Close(); <-done }()
			client.SetDeadline(time.Now().Add(5 * time.Second))
			reader := bufio.NewReader(client)
			if binary {
				_, err := client.Write(encodeBinaryFrame(binaryFrameHandshake, []byte{binaryProtocolVersion, 8, 8, 8, 0}))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := readBinaryFrame(reader); err != nil {
					t.Fatal(err)
				}
			}
			exchange := func(query string) string {
				t.Helper()
				if binary {
					_, err := client.Write(encodeBinaryFrame(binaryFrameRequest, buildRequestBody(t, "GRAPH_QUERY", binString("", query))))
					if err != nil {
						t.Fatal(err)
					}
					frame, err := readBinaryFrame(reader)
					if err != nil {
						t.Fatal(err)
					}
					return decodeResponseFrameBody(t, frame.Body)
				}
				if _, err := fmt.Fprintln(client, "GRAPH_QUERY "+query); err != nil {
					t.Fatal(err)
				}
				response, err := reader.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				return strings.TrimSuffix(response, "\n")
			}
			if response := exchange(clause); response != want {
				t.Fatalf("transport drift: %s != %s", response, want)
			}
			if response := exchange(`MATCH (id='hub')-[:knows]->(*) WHERE edge.modality = '\n' RETURN count`); !strings.HasPrefix(response, "ERROR") || strings.ContainsAny(response, "\r\n") {
				t.Fatalf("malformed literal broke the response line: %q", response)
			}
			if response := exchange(clause); response != want {
				t.Fatalf("connection lost alignment: %s", response)
			}
		})
	}
}

func TestGraphQueryJoinedGroupingAndTypedKeys(t *testing.T) {
	db := newAdaptiveTestDB(t, 1, true, 4096)
	for i, raw := range []string{`{"v":1}`, `{"v":"1"}`, `{"v":true}`, `{"v":null}`} {
		props := base64.StdEncoding.EncodeToString([]byte(raw))
		assertCommandPrefix(t, db, fmt.Sprintf("GRAPH_EDGE_SET from=hub to=u%d type=knows props=%s", i, props), "SUCCESS")
		assertCommandPrefix(t, db, fmt.Sprintf("GRAPH_EDGE_SET from=u%d to=org type=works_at", i), "SUCCESS")
	}
	response := assertCommandPrefix(t, db, "GRAPH_QUERY MATCH (id='hub')-[:knows]->(*) JOIN (to)-[:works_at]->(*) GROUP BY edge.props.v,join1.to.id", "SUCCESS")
	var groups []graphQueryGroup
	decodePayloadField(t, response, &groups)
	if len(groups) != 4 {
		t.Fatalf("typed group keys collided: %+v", groups)
	}
	for _, group := range groups {
		if group.Count != 1 || group.Key["join1.to.id"] != "org" {
			t.Fatalf("wrong joined group: %+v", group)
		}
	}
	// Projection never changes its input, including overlapping include paths.
	source := map[string]interface{}{"props": map[string]interface{}{"a": 1, "b": 2}}
	projected := graphQueryProject(source, []string{"props", "props.a"}, []string{"props.b"})
	if len(projected["props"].(map[string]interface{})) != 1 || len(source["props"].(map[string]interface{})) != 2 {
		t.Fatal("overlapping projection mutated its input")
	}
}

func BenchmarkGraphQueryStreamingJoin(b *testing.B) {
	cfg := defaultConfig().DatabaseDefaults
	db, err := NewDatabase("bench", b.TempDir(), nil, cfg, 256)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	for i := 0; i < 64; i++ {
		for _, cmd := range []string{
			fmt.Sprintf("GRAPH_EDGE_SET from=hub to=n%d type=knows", i),
			fmt.Sprintf("GRAPH_EDGE_SET from=n%d to=org type=works_at", i),
		} {
			if response, err := db.ExecuteCommand(cmd); err != nil || !strings.HasPrefix(response, "SUCCESS") {
				b.Fatalf("fixture: %s %v", response, err)
			}
		}
	}
	plan, err := parseGraphQuery("MATCH (id='hub')-[:knows]->(*) JOIN (to)-[:works_at]->(*) GROUP BY join1.to.id CACHE off")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.executeGraphRelationalQuery(plan); err != nil {
			b.Fatal(err)
		}
	}
}
