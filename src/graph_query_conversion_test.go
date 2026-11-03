package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestGraphQueryBoundFieldComparisons(t *testing.T) {
	for _, stride := range []int{1, 2} {
		t.Run(fmt.Sprint(stride), func(t *testing.T) {
			db := newAdaptiveTestDB(t, stride, true, 4096)
			props := base64.StdEncoding.EncodeToString([]byte(`{"n":2,"s":"2","b":true,"object":{}}`))
			for _, edge := range []string{"from=a to=b type=t", "from=b to=a type=u", "from=b to=c type=u"} {
				assertCommandPrefix(t, db, "GRAPH_EDGE_SET "+edge+" props="+props, "SUCCESS")
			}
			base := "GRAPH_QUERY MATCH (id='a')-[:t]->(*) JOIN (to)-[:u]->(*) WHERE "
			for _, tc := range []struct {
				where string
				count string
			}{
				{"from.id = @join1.to.id", "1"},
				{"from.id <> @join1.to.id", "1"},
				{"to.id = @join1.from.id AND from.id < @to.id", "2"},
				{"NOT from.id = @join1.to.id OR edge.weight = 0", "1"},
				{"from.id = @join1.to.id OR edge.weight = 1", "2"},
				{"edge.props.n = @join1.edge.props.n AND edge.props.b = @join1.edge.props.b", "2"},
				{"edge.props.n = @edge.props.s", "0"},
				{"edge.props.n != @edge.props.s", "2"},
				{"edge.props.missing != @edge.props.n", "0"},
				{"edge.props.n != @edge.props.missing", "0"},
				{"from.id != join1.to.id", "2"}, // Il RHS storico resta un letterale.
			} {
				response := assertCommandPrefix(t, db, base+tc.where+" RETURN count", "SUCCESS")
				if responseField(response, "matches") != tc.count {
					t.Errorf("%s: %s", tc.where, response)
				}
			}
		})
	}
	for _, clause := range []string{
		"WHERE from.id = @join1.to.id", "WHERE from.id = @bogus.id", "WHERE from.id IN (@to.id)",
		"WHERE from.label = @to.label", "RETURN DISTINCT", "RETURN to.id AS x,from.id AS x",
		"RETURN to.id,", "RETURN COUNT(DISTINCT *) AS n", "RETURN COUNT(*),to.id",
		"RETURN DISTINCT COUNT(*)", "RETURN SUM(edge.weight)", "RETURN to.id HOPS 2",
		"RETURN COUNT(*) CURSOR x01", "RETURN to.id INCLUDE FIELDS to", "RETURN MIN(edge.weight) GROUP BY edge.type",
	} {
		if _, err := parseGraphQuery("MATCH (id='a')-[:t]->(*) " + clause); err == nil {
			t.Errorf("accepted invalid query: %s", clause)
		}
	}
}

func TestGraphQueryJoinedContinuationIsComplete(t *testing.T) {
	for _, stride := range []int{1, 2} {
		t.Run(fmt.Sprint(stride), func(t *testing.T) {
			db := newAdaptiveTestDB(t, stride, true, 4096)
			for i := 0; i < 3; i++ {
				assertCommandPrefix(t, db, fmt.Sprintf("GRAPH_EDGE_SET from=hub to=p%d type=knows", i), "SUCCESS")
				for j := 0; j < 2; j++ {
					assertCommandPrefix(t, db, fmt.Sprintf("GRAPH_EDGE_SET from=p%d to=g%d type=works", i, j), "SUCCESS")
				}
			}
			for i := 0; i < 400; i++ {
				for j := 0; j < 2; j++ {
					assertCommandPrefix(t, db, fmt.Sprintf("GRAPH_EDGE_SET from=n%03d to=g%d type=member", i, j), "SUCCESS")
				}
				if i < 10 {
					assertCommandPrefix(t, db, fmt.Sprintf("GRAPH_EDGE_SET from=n%03d to=hub type=blocked", i), "SUCCESS")
				}
			}
			base := "GRAPH_QUERY MATCH (id='hub')-[:knows]->(*) JOIN (to)-[:works]->(*) JOIN (join1.to)<-[:member]-(*) INCLUDE (join2.from)-[:member]->(id='g0') EXCLUDE (join2.from)-[:blocked]->(id='hub') WHERE from.id != @join2.from.id RETURN to.id AS person,join1.to.id AS group,join2.from.id AS member CACHE off BUDGET 1048576"
			cursor, seen := "*", make(map[string]bool)
			for page := 0; page < 50; page++ {
				response := assertCommandPrefix(t, db, base+" LIMIT 127 CURSOR "+cursor, "SUCCESS")
				var rows []map[string]interface{}
				decodePayloadField(t, response, &rows)
				for _, row := range rows {
					raw, _ := json.Marshal(row)
					if seen[string(raw)] || row["person"] == nil || row["group"] == nil || row["member"] == nil {
						t.Fatalf("duplicate or incomplete binding: %s", raw)
					}
					seen[string(raw)] = true
				}
				cursor = responseField(response, "next_cursor")
				if (cursor == "*") != (responseField(response, "complete") == "true") {
					t.Fatalf("completion disagrees with continuation: %s", response)
				}
				if cursor == "*" {
					break
				}
			}
			if len(seen) != 2340 || cursor != "*" {
				t.Fatalf("lost joined rows: %d cursor=%s", len(seen), cursor)
			}
			// L'ultima pagina ha esattamente LIMIT righe: serve il lookahead.
			query := "GRAPH_QUERY MATCH (id='hub')-[:knows]->(*) JOIN (to)-[:works]->(*) RETURN count LIMIT 6"
			response := assertCommandPrefix(t, db, query, "SUCCESS,return=count,matches=6")
			if responseField(response, "complete") != "true" || responseField(response, "next_cursor") != "*" {
				t.Fatal(response)
			}
		})
	}
}

func TestGraphQueryContinuationRejectsChangedOrForgedState(t *testing.T) {
	db := newAdaptiveTestDB(t, 1, true, 4096)
	for _, edge := range []string{"from=a to=b type=t", "from=b to=c type=u", "from=b to=d type=u"} {
		assertCommandPrefix(t, db, "GRAPH_EDGE_SET "+edge, "SUCCESS")
	}
	query := "GRAPH_QUERY MATCH (id='a')-[:t]->(*) JOIN (to)-[:u]->(*) RETURN rows LIMIT 1"
	response := assertCommandPrefix(t, db, query, "SUCCESS")
	cursor := responseField(response, "next_cursor")
	if cursor == "*" || responseField(response, "complete") != "false" {
		t.Fatal(response)
	}
	for _, cmd := range []string{
		query + " CURSOR x01",
		strings.Replace(query, "RETURN rows", "RETURN edges", 1) + " CURSOR " + cursor,
		"GRAPH_QUERY MATCH (id='a')-[:t]->(*) RETURN count CURSOR " + cursor,
	} {
		if response, err := db.ExecuteCommand(cmd); err == nil || strings.Contains(response, "SUCCESS") {
			t.Fatalf("accepted invalid continuation: %s %v", response, err)
		}
	}
	raw, err := graphParseCursorToken(cursor)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if _, err := db.ExecuteCommand(query + " CURSOR " + graphCursorToken(raw)); err == nil {
		t.Fatal("accepted forged cursor")
	}
	// LIMIT e BUDGET possono cambiare fra pagine.
	response = assertCommandPrefix(t, db, strings.Replace(query, "LIMIT 1", "LIMIT 2 BUDGET 100", 1)+" CURSOR "+cursor, "SUCCESS,return=rows,matches=1")
	if responseField(response, "complete") != "true" {
		t.Fatal(response)
	}
	assertCommandPrefix(t, db, "GRAPH_NODE_SET id=b labels=changed", "SUCCESS")
	if _, err := db.ExecuteCommand(query + " CURSOR " + cursor); err == nil || err.Error() != "graph_query_cursor_stale" {
		t.Fatalf("graph mutation did not invalidate cursor: %v", err)
	}
	other := newAdaptiveTestDB(t, 1, true, 4096)
	if _, err := other.ExecuteCommand(query + " CURSOR " + cursor); err == nil || err.Error() != "invalid_graph_query_cursor" {
		t.Fatalf("another database accepted the cursor: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewDatabase(db.name, db.path, nil, db.settings, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	if _, err := reopened.ExecuteCommand(query + " CURSOR " + cursor); err == nil || err.Error() != "invalid_graph_query_cursor" {
		t.Fatalf("reopened database accepted the cursor: %v", err)
	}
}

func TestGraphQueryAnswerDistinctAndExactAggregates(t *testing.T) {
	for _, stride := range []int{1, 2} {
		t.Run(fmt.Sprint(stride), func(t *testing.T) {
			db := newAdaptiveTestDB(t, stride, true, 4096)
			for i := 0; i < 270; i++ {
				assertCommandPrefix(t, db, fmt.Sprintf("GRAPH_EDGE_SET from=hub to=n%03d type=t", i), "SUCCESS")
				for j := 0; j < 2; j++ {
					assertCommandPrefix(t, db, fmt.Sprintf("GRAPH_EDGE_SET from=n%03d to=g%d type=u", i, j), "SUCCESS")
				}
			}
			base := "GRAPH_QUERY MATCH (id='hub')-[:t]->(*) JOIN (to)-[:u]->(*) "
			query := base + "RETURN DISTINCT join1.to.id AS value LIMIT 1"
			response := assertCommandPrefix(t, db, query, "SUCCESS,return=values,matches=1")
			cursor := responseField(response, "next_cursor")
			var first, second []map[string]interface{}
			decodePayloadField(t, response, &first)
			response = assertCommandPrefix(t, db, query+" CURSOR "+cursor, "SUCCESS,return=values,matches=1")
			decodePayloadField(t, response, &second)
			if first[0]["value"] == second[0]["value"] || responseField(response, "complete") != "true" {
				t.Fatalf("distinct was not global: %v %v %s", first, second, response)
			}
			response = assertCommandPrefix(t, db, base+"RETURN COUNT(*) AS rows,COUNT(DISTINCT to.id) AS value,MIN(to.id) AS first,MAX(to.id) AS last LIMIT 1", "SUCCESS,return=aggregate,matches=1")
			var values []map[string]interface{}
			decodePayloadField(t, response, &values)
			if values[0]["rows"] != float64(540) || values[0]["value"] != float64(270) || values[0]["first"] != "n000" || values[0]["last"] != "n269" {
				t.Fatalf("aggregation truncated at LIMIT: %v", values)
			}
			response = assertCommandPrefix(t, db, base+"WHERE edge.weight = 0 RETURN COUNT(*) AS value,MIN(to.id) AS low,MAX(to.id) AS high", "SUCCESS")
			decodePayloadField(t, response, &values)
			if values[0]["value"] != float64(0) || values[0]["low"] != nil || values[0]["high"] != nil {
				t.Fatalf("wrong empty aggregate: %v", values)
			}
			for _, returns := range []string{"DISTINCT join1.to.id AS value", "COUNT(DISTINCT to.id) AS value", "MIN(to.id) AS value"} {
				response, err := db.ExecuteCommand(base + "RETURN " + returns + " BUDGET 10 LIMIT 1")
				if err == nil || err.Error() != "graph_query_budget_exceeded" || strings.Contains(response, "SUCCESS") {
					t.Fatalf("incomplete aggregate/distinct succeeded: %s %v", response, err)
				}
			}
		})
	}
}

func TestGraphQueryAggregateTypesAndTies(t *testing.T) {
	db := newAdaptiveTestDB(t, 2, true, 4096)
	for i, raw := range []string{`{"v":1}`, `{"v":2}`, `{"v":2}`, `{"v":null}`, `{}`} {
		props := base64.StdEncoding.EncodeToString([]byte(raw))
		assertCommandPrefix(t, db, fmt.Sprintf("GRAPH_NODE_SET id=n%d props=%s", i, props), "SUCCESS")
		assertCommandPrefix(t, db, fmt.Sprintf("GRAPH_EDGE_SET from=hub to=n%d type=t", i), "SUCCESS")
	}
	base := "GRAPH_QUERY MATCH (id='hub')-[:t]->(*) "
	response := assertCommandPrefix(t, db, base+"RETURN COUNT(to.props.v) AS n,COUNT(DISTINCT to.props.v) AS d,MIN(to.props.v) AS lo,MAX(to.props.v) AS hi", "SUCCESS")
	var values []map[string]interface{}
	decodePayloadField(t, response, &values)
	if values[0]["n"] != float64(3) || values[0]["d"] != float64(2) || values[0]["lo"] != float64(1) || values[0]["hi"] != float64(2) {
		t.Fatal(values)
	}
	response = assertCommandPrefix(t, db, base+"WHERE to.props.v = 2 RETURN DISTINCT to.id AS value", "SUCCESS,return=values,matches=2")
	assertCommandPrefix(t, db, "GRAPH_NODE_SET id=n0 props="+base64.StdEncoding.EncodeToString([]byte(`{"v":"9"}`)), "SUCCESS")
	if _, err := db.ExecuteCommand(base + "RETURN MAX(to.props.v)"); err == nil {
		t.Fatal("mixed numeric/string values silently aggregated")
	}
	budget := graphQueryDistinctBudget{entries: graphQueryMaxDistinct}
	if _, err := budget.add("extra", make(map[string]struct{})); err == nil {
		t.Fatal("distinct entry ceiling ignored")
	}
	budget = graphQueryDistinctBudget{bytes: graphQueryMaxOutputBytes}
	if _, err := budget.add("extra", make(map[string]struct{})); err == nil {
		t.Fatal("distinct byte ceiling ignored")
	}
}
