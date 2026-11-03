package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

const graphQueryMaxDistinct = 65536

type graphQuerySelect struct {
	Field     *graphQueryField
	Aggregate string
	Distinct  bool
	Alias     string
}

func (plan *graphQueryPlan) hasJoins() bool {
	for _, rel := range plan.Relations {
		if rel.Kind == "JOIN" {
			return true
		}
	}
	return false
}

// La selezione usa i campi legati del WHERE, non i percorsi JSON di INCLUDE
// FIELDS. Le vecchie modalità RETURN mantengono forma e significato.
func graphQueryParseReturn(plan *graphQueryPlan, raw string, joins int) error {
	if mode, err := parseGraphReturnMode(raw); err == nil {
		plan.Return = mode
		return nil
	}
	tokens, err := graphQueryLex(raw)
	if err != nil {
		return err
	}
	p := graphQueryExprParser{tokens: tokens, joins: joins}
	plan.Distinct = p.take("DISTINCT")
	aliases := make(map[string]bool)
	aggregates := 0
	for p.pos < len(tokens) {
		item := graphQuerySelect{}
		if p.pos+1 < len(tokens) && tokens[p.pos].Kind == 'w' && tokens[p.pos+1].Text == "(" {
			item.Aggregate = strings.ToLower(tokens[p.pos].Text)
			switch item.Aggregate {
			case "count", "min", "max":
			default:
				return fmt.Errorf("unsupported_aggregate:%s", item.Aggregate)
			}
			aggregates++
			p.pos += 2
			item.Distinct = p.take("DISTINCT")
		}
		if item.Aggregate == "count" && p.take("*") {
			if item.Distinct {
				return fmt.Errorf("count_distinct_requires_field")
			}
		} else {
			if p.pos >= len(tokens) || tokens[p.pos].Kind != 'w' {
				return fmt.Errorf("expected_return_field")
			}
			ref, err := graphQueryParseField(tokens[p.pos].Text, joins)
			if err != nil {
				return err
			}
			p.pos++
			item.Field, item.Alias = &ref, ref.Name
		}
		if item.Aggregate != "" {
			if !p.take(")") {
				return fmt.Errorf("expected_aggregate_closing_parenthesis")
			}
			item.Alias = item.Aggregate
		}
		if p.take("AS") {
			if p.pos >= len(tokens) || tokens[p.pos].Kind != 'w' || !graphQueryIdentifier(tokens[p.pos].Text) {
				return fmt.Errorf("invalid_return_alias")
			}
			item.Alias = tokens[p.pos].Text
			p.pos++
		}
		if aliases[item.Alias] {
			return fmt.Errorf("duplicate_return_alias:%s", item.Alias)
		}
		aliases[item.Alias] = true
		plan.Select = append(plan.Select, item)
		if len(plan.Select) > graphQueryMaxFields {
			return fmt.Errorf("too_many_return_fields")
		}
		if p.pos == len(tokens) {
			break
		}
		if !p.take(",") || p.pos == len(tokens) {
			return fmt.Errorf("expected_return_comma_or_end")
		}
	}
	if len(plan.Select) == 0 {
		return fmt.Errorf("expected_return_field")
	}
	plan.Return = graphReturnValues
	if aggregates > 0 {
		if aggregates != len(plan.Select) || plan.Distinct {
			return fmt.Errorf("aggregate_return_requires_only_aggregates")
		}
		plan.Return = graphReturnAggregate
	}
	return nil
}

func graphQueryScalarKind(value interface{}) byte {
	switch value.(type) {
	case string:
		return 's'
	case float64:
		return 'n'
	case bool:
		return 'b'
	}
	return 0
}

// I riferimenti espliciti non applicano le coercizioni storiche dei letterali.
func graphQueryCompareFields(actual interface{}, op string, expected interface{}) (bool, error) {
	a, b := graphQueryScalarKind(actual), graphQueryScalarKind(expected)
	if a == 0 || b == 0 {
		return false, fmt.Errorf("field_comparison_requires_scalars")
	}
	if a != b {
		return op == "!=", nil
	}
	if a == 'b' && op != "=" && op != "!=" {
		return false, fmt.Errorf("unordered_predicate_type")
	}
	return graphQueryCompare(actual, op, graphQueryLiteral{Value: expected, Kind: b}, graphQueryField{}), nil
}

func (exec *graphQueryExecution) selectRow(row []*GraphEdgeRecord) (map[string]interface{}, error) {
	result := make(map[string]interface{}, len(exec.plan.Select))
	for _, item := range exec.plan.Select {
		value, _, err := exec.field(*item.Field, row)
		if err != nil {
			return nil, err
		}
		result[item.Alias] = value
	}
	return result, nil
}

// Un unico tetto condiviso limita tutte le strutture DISTINCT della query.
type graphQueryDistinctBudget struct {
	entries int
	bytes   int
}

func (budget *graphQueryDistinctBudget) add(key string, seen map[string]struct{}) (bool, error) {
	if _, exists := seen[key]; exists {
		return false, nil
	}
	budget.entries++
	budget.bytes += len(key)
	if budget.entries > graphQueryMaxDistinct || budget.bytes > graphQueryMaxOutputBytes {
		return false, fmt.Errorf("graph_query_distinct_capacity_exceeded")
	}
	seen[key] = struct{}{}
	return true, nil
}

type graphQueryAccumulator struct {
	count int
	value interface{}
	seen  map[string]struct{}
}

func (exec *graphQueryExecution) aggregate() ([]json.RawMessage, error) {
	states := make([]graphQueryAccumulator, len(exec.plan.Select))
	budget := graphQueryDistinctBudget{}
	for i, item := range exec.plan.Select {
		if item.Distinct && item.Aggregate == "count" {
			states[i].seen = make(map[string]struct{})
		}
	}
	err := exec.walkBindings(nil, func(row []*GraphEdgeRecord, _ [][]byte) (bool, error) {
		if err := exec.spend(1); err != nil {
			return false, err
		}
		for i, item := range exec.plan.Select {
			state := &states[i]
			var value interface{}
			if item.Field != nil {
				var exists bool
				var err error
				value, exists, err = exec.field(*item.Field, row)
				if err != nil {
					return false, err
				}
				if !exists {
					continue
				}
			}
			if item.Aggregate == "count" {
				if state.seen != nil {
					raw, err := json.Marshal(value)
					if err != nil {
						return false, err
					}
					fresh, err := budget.add(string(raw), state.seen)
					if err != nil {
						return false, err
					}
					if !fresh {
						continue
					}
				}
				state.count++
				continue
			}
			kind := graphQueryScalarKind(value)
			if kind != 'n' && kind != 's' || state.value != nil && graphQueryScalarKind(state.value) != kind {
				return false, fmt.Errorf("aggregate_requires_uniform_numbers_or_strings:%s", item.Alias)
			}
			op := "<"
			if item.Aggregate == "max" {
				op = ">"
			}
			if state.value == nil || graphQueryCompare(value, op, graphQueryLiteral{Value: state.value, Kind: kind}, graphQueryField{}) {
				state.value = value
			}
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	result := make(map[string]interface{}, len(states))
	for i, state := range states {
		item := exec.plan.Select[i]
		result[item.Alias] = state.value
		if item.Aggregate == "count" {
			result[item.Alias] = state.count
		}
	}
	raw, err := json.Marshal(result)
	if len(raw) > graphQueryMaxOutputBytes {
		return nil, fmt.Errorf("graph_query_output_capacity_exceeded")
	}
	return []json.RawMessage{raw}, err
}
