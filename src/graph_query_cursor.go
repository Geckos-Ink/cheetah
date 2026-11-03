package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const graphQueryMaxCursorBytes = 24 << 10

type graphQueryContinuation struct {
	Query  string   `json:"q"`
	Epoch  uint64   `json:"e"`
	Keys   [][]byte `json:"k,omitempty"`
	Offset int      `json:"o,omitempty"`
}

func graphQueryFingerprint(plan *graphQueryPlan) string {
	copy := *plan
	copy.Cursor, copy.Limit, copy.Budget, copy.CacheOff = nil, 0, 0, false
	raw, _ := json.Marshal(copy)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func (db *Database) graphQueryReadContinuation(plan *graphQueryPlan) (*graphQueryContinuation, error) {
	state := &graphQueryContinuation{Query: graphQueryFingerprint(plan), Epoch: db.graphCache.currentEpoch()}
	if len(plan.Cursor) == 0 {
		return state, nil
	}
	if len(plan.Cursor) <= 36 || len(plan.Cursor) > graphQueryMaxCursorBytes || !bytes.HasPrefix(plan.Cursor, []byte("CHQ1")) {
		return nil, fmt.Errorf("invalid_graph_query_cursor")
	}
	message, signature := plan.Cursor[36:], plan.Cursor[4:36]
	mac := hmac.New(sha256.New, db.graphQueryCursorKey[:])
	mac.Write(message)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return nil, fmt.Errorf("invalid_graph_query_cursor")
	}
	var decoded graphQueryContinuation
	if err := json.Unmarshal(message, &decoded); err != nil || decoded.Query != state.Query {
		return nil, fmt.Errorf("graph_query_cursor_query_mismatch")
	}
	if decoded.Epoch != state.Epoch {
		return nil, fmt.Errorf("graph_query_cursor_stale")
	}
	if plan.Distinct {
		if len(decoded.Keys) != 0 || decoded.Offset < 1 || decoded.Offset > graphQueryMaxDistinct {
			return nil, fmt.Errorf("invalid_graph_query_cursor")
		}
	} else {
		n := 1
		for _, rel := range plan.Relations {
			if rel.Kind == "JOIN" {
				n++
			}
		}
		if len(decoded.Keys) != n || decoded.Offset != 0 {
			return nil, fmt.Errorf("invalid_graph_query_cursor")
		}
	}
	return &decoded, nil
}

func (db *Database) graphQueryWriteContinuation(state *graphQueryContinuation) ([]byte, error) {
	message, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	if len(message)+36 > graphQueryMaxCursorBytes {
		return nil, fmt.Errorf("graph_query_cursor_capacity_exceeded")
	}
	mac := hmac.New(sha256.New, db.graphQueryCursorKey[:])
	mac.Write(message)
	return append(append([]byte("CHQ1"), mac.Sum(nil)...), message...), nil
}

// Il ramo salvato viene riletto una sola volta per ricostruire gli antenati;
// poi si riparte dopo la sua chiave. Nessun OFFSET sulle righe già prodotte.
func (exec *graphQueryExecution) scanBindingLevel(prefix []byte, resume [][]byte, visit func(GraphEdgeRecord, []byte, [][]byte, bool) (bool, error)) error {
	var cursor []byte
	if len(resume) > 0 {
		cursor = resume[0]
		if !bytes.HasPrefix(cursor, prefix) {
			return fmt.Errorf("invalid_graph_query_cursor_path")
		}
		if err := exec.spend(1); err != nil {
			return err
		}
		key, found, err := exec.db.getPairPayload(cursor)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("graph_query_cursor_stale")
		}
		edge, found, err := exec.db.graphGetEdgeByPairKey(key)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("graph_query_cursor_stale")
		}
		adjacency := graphAdjOutPairKey(edge.From, edge.Type, edge.To, edge.Directed)
		if bytes.HasPrefix(prefix, []byte(graphAdjInPrefix)) {
			adjacency = graphAdjInPairKey(edge.To, edge.Type, edge.From, edge.Directed)
		}
		if !bytes.Equal(adjacency, cursor) {
			return fmt.Errorf("invalid_graph_query_cursor_path")
		}
		more, err := visit(edge, cursor, resume[1:], true)
		if err != nil || !more {
			return err
		}
	}
	_, err := exec.scan(prefix, cursor, func(edge GraphEdgeRecord, key []byte) (bool, error) {
		return visit(edge, key, nil, false)
	})
	return err
}

type graphQueryBindingVisitor func([]*GraphEdgeRecord, [][]byte) (bool, error)

func (exec *graphQueryExecution) expandBindings(row []*GraphEdgeRecord, index int, path, resume [][]byte, skip bool, emit graphQueryBindingVisitor) (bool, error) {
	ok, known, err := exec.ready(exec.plan.Filter, row)
	if err != nil || known && !ok {
		return true, err
	}
	if index == len(exec.plan.Relations) {
		if skip {
			return true, nil
		}
		return emit(row, path)
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
		return exec.expandBindings(row, index+1, path, resume, skip, emit)
	}
	more := true
	err = exec.scanBindingLevel(graphQueryAdjPrefix(id, rel.EdgeType, rel.Direction, rel.Target), resume, func(edge GraphEdgeRecord, key []byte, tail [][]byte, saved bool) (bool, error) {
		endpoint := edge.To
		if rel.Direction == "in" {
			endpoint = edge.From
		}
		ok, err := exec.matchNode(endpoint, rel.Target)
		if err != nil || !ok {
			return true, err
		}
		more, err = exec.expandBindings(append(row, &edge), index+1, append(path, key), tail, saved, emit)
		return more, err
	})
	return more, err
}

func (exec *graphQueryExecution) walkBindings(resume [][]byte, emit graphQueryBindingVisitor) error {
	plan := exec.plan
	ok, err := exec.matchNode(plan.Left.ID, plan.Left)
	if err != nil || !ok {
		return err
	}
	return exec.scanBindingLevel(graphQueryAdjPrefix(plan.Left.ID, plan.EdgeType, plan.Direction, plan.Right), resume, func(edge GraphEdgeRecord, key []byte, tail [][]byte, saved bool) (bool, error) {
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
		return exec.expandBindings([]*GraphEdgeRecord{&edge}, 0, [][]byte{key}, tail, saved, emit)
	})
}
