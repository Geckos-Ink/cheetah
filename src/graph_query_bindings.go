package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

func (exec *graphQueryExecution) bindingValue(row []*GraphEdgeRecord) (interface{}, error) {
	plan := exec.plan
	if plan.Return == graphReturnValues {
		return exec.selectRow(row)
	}
	var value interface{}
	switch plan.Return {
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
			fields := map[string]interface{}{"from": edge.From, "to": edge.To, "weight": edge.Weight}
			if edge.Type != "" {
				fields["type"] = edge.Type
			}
			value = fields
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
	return value, nil
}

func (exec *graphQueryExecution) distinctBindings(offset int) ([]json.RawMessage, int, error) {
	seen := make(map[string]struct{})
	budget := graphQueryDistinctBudget{}
	err := exec.walkBindings(nil, func(row []*GraphEdgeRecord, _ [][]byte) (bool, error) {
		if err := exec.spend(1); err != nil {
			return false, err
		}
		value, err := exec.selectRow(row)
		if err != nil {
			return false, err
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return false, err
		}
		_, err = budget.add(string(raw), seen)
		return err == nil, err
	})
	if err != nil {
		return nil, 0, err
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if offset > len(keys) {
		return nil, 0, fmt.Errorf("invalid_graph_query_cursor_offset")
	}
	end := offset + exec.plan.Limit
	if end > len(keys) {
		end = len(keys)
	}
	output := make([]json.RawMessage, 0, end-offset)
	for _, key := range keys[offset:end] {
		output = append(output, json.RawMessage(key))
	}
	if end == len(keys) {
		end = 0
	}
	return output, end, nil
}

func (db *Database) executeGraphBindingQuery(plan *graphQueryPlan) (string, error) {
	state, err := db.graphQueryReadContinuation(plan)
	if err != nil {
		return "", err
	}
	exec := newGraphQueryExecution(db, plan)
	output := []json.RawMessage{}
	matches, outputBytes, matchedRows := 0, 0, 0
	more := false
	unique := make(map[string]struct{})
	switch {
	case plan.Return == graphReturnAggregate:
		output, err = exec.aggregate()
		matches = 1
	case plan.Distinct:
		output, state.Offset, err = exec.distinctBindings(state.Offset)
		matches, more = len(output), state.Offset > 0
	default:
		err = exec.walkBindings(state.Keys, func(row []*GraphEdgeRecord, path [][]byte) (bool, error) {
			if err := exec.spend(1); err != nil {
				return false, err
			}
			// Una riga di lookahead distingue LIMIT dall'esaurimento effettivo.
			if matchedRows == plan.Limit {
				more = true
				return false, nil
			}
			matchedRows++
			state.Keys = make([][]byte, len(path))
			for i, key := range path {
				state.Keys[i] = append([]byte(nil), key...)
			}
			if plan.Return == graphReturnCount {
				return true, nil
			}
			if plan.Return == graphReturnNodes {
				for _, id := range []string{row[0].From, row[0].To} {
					if _, exists := unique[id]; !exists {
						unique[id] = struct{}{}
						outputBytes += len(id)
					}
				}
			} else {
				value, err := exec.bindingValue(row)
				if err != nil {
					return false, err
				}
				raw, err := json.Marshal(value)
				if err != nil {
					return false, err
				}
				output = append(output, raw)
				outputBytes += len(raw)
			}
			if outputBytes > graphQueryMaxOutputBytes {
				return false, fmt.Errorf("graph_query_output_capacity_exceeded")
			}
			return true, nil
		})
		matches = matchedRows
	}
	if err != nil {
		return "", err
	}
	if db.graphCache.currentEpoch() != state.Epoch {
		return "", fmt.Errorf("graph_query_graph_changed")
	}
	var cursor []byte
	if more {
		cursor, err = db.graphQueryWriteContinuation(state)
		if err != nil {
			return "", err
		}
	}
	response := fmt.Sprintf("SUCCESS,return=%s,matches=%d,next_cursor=%s,complete=%t", plan.Return, matches, graphCursorToken(cursor), !more)
	if plan.Return == graphReturnCount {
		return response, nil
	}
	var payload string
	if plan.Return == graphReturnNodes {
		nodes := make([]string, 0, len(unique))
		for id := range unique {
			nodes = append(nodes, id)
		}
		sort.Strings(nodes)
		payload, err = graphEncodeJSON(nodes)
		response = fmt.Sprintf("SUCCESS,return=%s,matches=%d,next_cursor=%s,complete=%t", plan.Return, len(nodes), graphCursorToken(cursor), !more)
	} else {
		payload, err = graphEncodeJSON(output)
	}
	if err != nil {
		return "", err
	}
	return response + ",payload=" + payload, nil
}
