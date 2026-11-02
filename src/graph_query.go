package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

const graphQueryMaxOutputBytes = 8 << 20

type graphQueryExecution struct {
	db       *Database
	plan     *graphQueryPlan
	nodes    map[string]*GraphNodeRecord
	work     int
	cacheOps int
}

func newGraphQueryExecution(db *Database, plan *graphQueryPlan) *graphQueryExecution {
	return &graphQueryExecution{db: db, plan: plan, nodes: make(map[string]*GraphNodeRecord)}
}

func (exec *graphQueryExecution) spend(n int) error {
	exec.work += n
	if exec.work > exec.plan.Budget {
		return fmt.Errorf("graph_query_budget_exceeded")
	}
	return nil
}

func graphQueryAdjPrefix(id, typ, direction string, target graphQueryNodePattern) []byte {
	prefix := graphAdjOutScanPrefix(id, typ)
	if direction == "in" {
		prefix = graphAdjInScanPrefix(id, typ)
	}
	// Con tipo e destinazione noti si cerca soltanto la coppia, non il vicinato.
	if typ != "" && target.ID != "" {
		prefix = append(prefix, []byte(graphEncodeSegment(target.ID)+"/")...)
	}
	return prefix
}

// scan mantiene una sola pagina di riferimenti e un arco decodificato alla
// volta. Il cursore è l'ultima riga consumata, anche quando il filtro scarta
// quasi tutto. Il budget comprende le pagine, le righe e le letture dei nodi.
func (exec *graphQueryExecution) scan(prefix, cursor []byte, visit func(GraphEdgeRecord, []byte) (bool, error)) ([]byte, error) {
	for {
		if err := exec.spend(1); err != nil {
			return nil, err
		}
		n := 128
		if exec.db.resources != nil && exec.db.resources.Snapshot().MemoryPressure >= 0.85 {
			n = 32
		}
		if available := exec.plan.Budget - exec.work; available > 0 && available < n {
			n = available
		}
		results, next, err := exec.db.PairScanWithOptions(prefix, n, cursor, true)
		if err != nil {
			return nil, err
		}
		for i, res := range results {
			if err := exec.spend(1); err != nil {
				return nil, err
			}
			key, err := exec.db.readValuePayload(res.Key)
			if err != nil {
				return nil, err
			}
			edge, found, err := exec.db.graphGetEdgeByPairKey(key)
			if err != nil {
				return nil, err
			}
			if !found {
				continue
			}
			// Un indice derivato può rimanere indietro durante una mutazione.
			// La riga autorevole deve ancora descrivere questa esatta adiacenza.
			adjacency := graphAdjOutPairKey(edge.From, edge.Type, edge.To, edge.Directed)
			if bytes.HasPrefix(prefix, []byte(graphAdjInPrefix)) {
				adjacency = graphAdjInPairKey(edge.To, edge.Type, edge.From, edge.Directed)
			}
			if !bytes.Equal(adjacency, res.Value) {
				continue
			}
			more, err := visit(edge, res.Value)
			if err != nil {
				return nil, err
			}
			if !more {
				if i+1 < len(results) {
					return append([]byte(nil), res.Value...), nil
				}
				return next, nil
			}
		}
		if len(next) == 0 || len(results) == 0 {
			return nil, nil
		}
		cursor = next
	}
}

func (exec *graphQueryExecution) matchNode(id string, pattern graphQueryNodePattern) (bool, error) {
	if pattern.ID != "" && id != pattern.ID {
		return false, nil
	}
	if pattern.Label == "" {
		return true, nil
	}
	node, err := exec.node(id)
	return node != nil && graphContainsLabel(node.Labels, pattern.Label), err
}

// ready respinge presto i congiunti già valutabili. Un ramo OR/NOT con campi
// futuri resta sconosciuto: non si può usarlo per potare un JOIN.
func (exec *graphQueryExecution) ready(expr *graphQueryExpr, row []*GraphEdgeRecord) (bool, bool, error) {
	if expr == nil {
		return true, true, nil
	}
	if expr.Op == "" {
		if expr.Predicate.Join >= len(row) {
			return false, false, nil
		}
		ok, err := exec.matches(expr, row)
		return ok, true, err
	}
	left, known, err := exec.ready(expr.Left, row)
	if err != nil {
		return false, false, err
	}
	if expr.Op == "not" {
		return !left, known, nil
	}
	if known && (expr.Op == "and" && !left || expr.Op == "or" && left) {
		return left, true, nil
	}
	right, rightKnown, err := exec.ready(expr.Right, row)
	if rightKnown && (expr.Op == "and" && !right || expr.Op == "or" && right) {
		return right, true, err
	}
	if expr.Op == "and" {
		return left && right, known && rightKnown, err
	}
	return left || right, known && rightKnown, err
}

func (exec *graphQueryExecution) expand(row []*GraphEdgeRecord, index int, emit func([]*GraphEdgeRecord) (bool, error)) (bool, error) {
	ok, known, err := exec.ready(exec.plan.Filter, row)
	if err != nil || known && !ok {
		return true, err
	}
	if index == len(exec.plan.Relations) {
		return emit(row)
	}
	rel := exec.plan.Relations[index]
	value, _, err := exec.field(rel.Source, row)
	if err != nil {
		return false, err
	}
	id := value.(string)
	if rel.Kind != "JOIN" {
		found, err := exec.associationExists(id, rel)
		if err != nil || found == (rel.Kind == "EXCLUDE") {
			return true, err
		}
		return exec.expand(row, index+1, emit)
	}
	more := true
	_, err = exec.scan(graphQueryAdjPrefix(id, rel.EdgeType, rel.Direction, rel.Target), nil, func(edge GraphEdgeRecord, _ []byte) (bool, error) {
		endpoint := edge.To
		if rel.Direction == "in" {
			endpoint = edge.From
		}
		ok, err := exec.matchNode(endpoint, rel.Target)
		if err != nil || !ok {
			return true, err
		}
		more, err = exec.expand(append(row, &edge), index+1, emit)
		return more, err
	})
	return more, err
}

type graphQueryGroup struct {
	Key       map[string]interface{} `json:"key"`
	Count     int                    `json:"count"`
	WeightSum float64                `json:"weight_sum"`
	WeightMin float64                `json:"weight_min"`
	WeightMax float64                `json:"weight_max"`
	WeightAvg float64                `json:"weight_avg"`
}

func (db *Database) executeGraphRelationalQuery(plan *graphQueryPlan) (string, error) {
	exec := newGraphQueryExecution(db, plan)
	ok, err := exec.matchNode(plan.Left.ID, plan.Left)
	if err != nil {
		return "", err
	}
	if !ok {
		return graphQueryEmptyResponse(plan), nil
	}
	var output []json.RawMessage
	groups := make(map[string]*graphQueryGroup)
	groupBytes, outputBytes, matches := 0, 0, 0
	unique := make(map[string]struct{})
	joins := false
	for _, rel := range plan.Relations {
		joins = joins || rel.Kind == "JOIN"
	}
	emit := func(row []*GraphEdgeRecord) (bool, error) {
		if err := exec.spend(1); err != nil {
			return false, err
		}
		if len(plan.GroupBy) > 0 {
			values := make([]interface{}, 0, len(plan.GroupBy))
			for _, ref := range plan.GroupBy {
				value, _, err := exec.field(ref, row)
				if err != nil {
					return false, err
				}
				switch value.(type) {
				case nil, string, float64, bool:
				default:
					return false, fmt.Errorf("group_field_must_be_scalar:%s", ref.Name)
				}
				values = append(values, value)
			}
			raw, err := json.Marshal(values)
			if err != nil {
				return false, err
			}
			key := string(raw)
			group := groups[key]
			if group == nil {
				groupBytes += len(key)
				if len(groups) >= graphMaxLimit || groupBytes > 1<<20 {
					return false, fmt.Errorf("graph_query_group_capacity_exceeded")
				}
				group = &graphQueryGroup{Key: make(map[string]interface{}), WeightMin: row[0].Weight, WeightMax: row[0].Weight}
				for i, ref := range plan.GroupBy {
					group.Key[ref.Name] = values[i]
				}
				groups[key] = group
			}
			group.Count++
			group.WeightSum += row[0].Weight
			if math.IsInf(group.WeightSum, 0) {
				return false, fmt.Errorf("graph_query_aggregate_overflow")
			}
			group.WeightMin = math.Min(group.WeightMin, row[0].Weight)
			group.WeightMax = math.Max(group.WeightMax, row[0].Weight)
			return true, nil
		}
		matches++
		var value interface{}
		switch plan.Return {
		case graphReturnCount:
			return matches < plan.Limit, nil
		case graphReturnNodes:
			for _, id := range []string{row[0].From, row[0].To} {
				if _, exists := unique[id]; !exists {
					outputBytes += len(id)
					unique[id] = struct{}{}
				}
			}
			if outputBytes > graphQueryMaxOutputBytes {
				return false, fmt.Errorf("graph_query_output_capacity_exceeded")
			}
			return matches < plan.Limit, nil
		case graphReturnRows:
			fields := make(map[string]interface{}, len(row))
			for i, edge := range row {
				name := "edge"
				if i > 0 {
					name = "join" + strconv.Itoa(i)
				}
				fields[name] = edge
				if len(plan.IncludeFields)+len(plan.ExcludeFields) > 0 {
					fields[name] = graphQueryEdgeFields(edge)
				}
			}
			value = fields
		case graphReturnPaths:
			edge := row[0]
			value = graphPathView{From: edge.From, To: edge.To, Type: edge.Type, Weight: edge.Weight}
			if len(plan.IncludeFields)+len(plan.ExcludeFields) > 0 {
				value = map[string]interface{}{"from": edge.From, "to": edge.To, "weight": edge.Weight}
				if edge.Type != "" {
					value.(map[string]interface{})["type"] = edge.Type
				}
			}
		default:
			value = row[0]
			if len(plan.IncludeFields)+len(plan.ExcludeFields) > 0 {
				value = graphQueryEdgeFields(row[0])
			}
		}
		if len(plan.IncludeFields)+len(plan.ExcludeFields) > 0 {
			value = graphQueryProject(value.(map[string]interface{}), plan.IncludeFields, plan.ExcludeFields)
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return false, err
		}
		outputBytes += len(raw)
		if outputBytes > graphQueryMaxOutputBytes {
			return false, fmt.Errorf("graph_query_output_capacity_exceeded")
		}
		output = append(output, raw)
		return matches < plan.Limit, nil
	}
	cursor, err := exec.scan(graphQueryAdjPrefix(plan.Left.ID, plan.EdgeType, plan.Direction, plan.Right), plan.Cursor, func(edge GraphEdgeRecord, _ []byte) (bool, error) {
		endpoint := edge.To
		if plan.Direction == "in" {
			endpoint = edge.From
		}
		ok, err := exec.matchNode(endpoint, plan.Right)
		if err != nil || !ok {
			return true, err
		}
		if plan.CostLimit > 0 && graphEdgeTraversalCost(edge.Weight) > plan.CostLimit {
			return true, nil
		}
		return exec.expand([]*GraphEdgeRecord{&edge}, 0, emit)
	})
	if err != nil {
		return "", err
	}
	if joins || len(plan.GroupBy) > 0 {
		cursor = nil
	}
	if plan.Return == graphReturnCount {
		return fmt.Sprintf("SUCCESS,return=count,matches=%d,next_cursor=%s", matches, graphCursorToken(cursor)), nil
	}
	var payload string
	if plan.Return == graphReturnNodes {
		nodes := make([]string, 0, len(unique))
		for id := range unique {
			nodes = append(nodes, id)
		}
		sort.Strings(nodes)
		matches = len(nodes)
		payload, err = graphEncodeJSON(nodes)
	} else if plan.Return == graphReturnGroups {
		keys := make([]string, 0, len(groups))
		for key := range groups {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if len(keys) > plan.Limit {
			keys = keys[:plan.Limit]
		}
		result := make([]*graphQueryGroup, 0, len(keys))
		for _, key := range keys {
			group := groups[key]
			group.WeightAvg = group.WeightSum / float64(group.Count)
			result = append(result, group)
		}
		matches = len(result)
		payload, err = graphEncodeJSON(result)
	} else {
		if output == nil {
			output = []json.RawMessage{}
		}
		payload, err = graphEncodeJSON(output)
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("SUCCESS,return=%s,matches=%d,next_cursor=%s,payload=%s", plan.Return, matches, graphCursorToken(cursor), payload), nil
}

func graphQueryEmptyResponse(plan *graphQueryPlan) string {
	response := fmt.Sprintf("SUCCESS,return=%s,matches=0,next_cursor=*", plan.Return)
	if plan.Return != graphReturnCount {
		response += ",payload=W10="
	}
	return response
}

func graphQueryEdgeFields(edge *GraphEdgeRecord) map[string]interface{} {
	fields := map[string]interface{}{"id": edge.ID, "from": edge.From, "to": edge.To, "directed": edge.Directed, "weight": edge.Weight}
	if edge.Type != "" {
		fields["type"] = edge.Type
	}
	if edge.Confidence != nil {
		fields["confidence"] = *edge.Confidence
	}
	for name, value := range map[string]string{"modality": edge.Modality, "ambiguity": edge.Ambiguity, "created_at": edge.CreatedAt, "updated_at": edge.UpdatedAt} {
		if value != "" {
			fields[name] = value
		}
	}
	if len(edge.Props) > 0 {
		fields["props"] = edge.Props
	}
	return fields
}

// Le mappe dei payload restano immutabili. La proiezione copia soltanto i rami
// modificati: togliere props.secret non duplica tutte le proprietà di un arco.
func graphQueryProject(source map[string]interface{}, include, exclude []string) map[string]interface{} {
	result := make(map[string]interface{})
	if len(include) == 0 {
		for key, value := range source {
			result[key] = value
		}
	} else {
		for _, path := range include {
			parts := strings.Split(path, ".")
			if value, ok := graphLookupPropertyValue(source, parts); ok {
				graphQuerySetProjection(result, parts, value)
			}
		}
	}
	for _, path := range exclude {
		graphQueryDropProjection(result, strings.Split(path, "."))
	}
	return result
}

func graphQuerySetProjection(target map[string]interface{}, path []string, value interface{}) {
	if len(path) == 1 {
		target[path[0]] = value
		return
	}
	next := make(map[string]interface{})
	if old, ok := target[path[0]].(map[string]interface{}); ok {
		for k, v := range old {
			next[k] = v
		}
	}
	target[path[0]] = next
	graphQuerySetProjection(next, path[1:], value)
}

func graphQueryDropProjection(target map[string]interface{}, path []string) {
	if len(path) == 1 {
		delete(target, path[0])
		return
	}
	old, ok := target[path[0]].(map[string]interface{})
	if !ok {
		return
	}
	next := make(map[string]interface{}, len(old))
	for k, v := range old {
		next[k] = v
	}
	target[path[0]] = next
	graphQueryDropProjection(next, path[1:])
}
