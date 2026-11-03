package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	graphQueryDefaultBudget = 16384
	graphQueryMaxBudget     = 1048576
	graphQueryMaxFrontier   = 4096
	graphQueryMaxRelations  = 8
	graphQueryMaxFields     = 32
	graphQueryMaxText       = 65536
)

type graphQueryRelation struct {
	Kind      string
	Source    graphQueryField
	Direction string
	EdgeType  string
	Target    graphQueryNodePattern
	Join      int
}

// Le associazioni partono sempre da un endpoint già legato: nessun prodotto
// cartesiano e nessuna scansione globale per risolvere un JOIN.
func parseGraphQuery(raw string) (*graphQueryPlan, error) {
	if len(raw) > graphQueryMaxText {
		return nil, fmt.Errorf("query_too_large")
	}
	raw = strings.TrimSpace(raw)
	if !graphQueryKeywordAt(raw, 0, "MATCH") {
		return nil, fmt.Errorf("query_must_start_with_MATCH")
	}
	left, direction, edgeType, right, rest, err := graphQueryParsePattern(strings.TrimSpace(raw[5:]))
	if err != nil {
		return nil, err
	}
	anchor, err := parseGraphQueryNodePattern(left)
	if err != nil {
		return nil, fmt.Errorf("invalid_left_node:%w", err)
	}
	if anchor.Wildcard || anchor.ID == "" {
		return nil, fmt.Errorf("left_node_must_be_anchored_by_id")
	}
	plan := &graphQueryPlan{
		Left: anchor, Right: right, Direction: direction, EdgeType: edgeType,
		MinHops: 1, MaxHops: 1, BranchLimit: graphDefaultBranch,
		Return: graphReturnEdges, Limit: graphDefaultLimit, Budget: graphQueryDefaultBudget,
	}
	clauses, err := graphQuerySplitClauses(rest)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	joins := 0
	var where, group, returns string
	for _, clause := range clauses {
		key, value := clause[0], clause[1]
		if key == "JOIN" || key == "INCLUDE" || key == "EXCLUDE" {
			if len(plan.Relations) >= graphQueryMaxRelations {
				return nil, fmt.Errorf("too_many_relations")
			}
			source, dir, typ, target, trailing, err := graphQueryParsePattern(value)
			if err != nil || strings.TrimSpace(trailing) != "" {
				return nil, fmt.Errorf("invalid_%s_pattern", strings.ToLower(key))
			}
			ref, err := graphQueryParseField(strings.TrimSpace(source)+".id", joins)
			if err != nil || (ref.Scope != "from" && ref.Scope != "to") || ref.Field != "id" {
				return nil, fmt.Errorf("relationship_source_must_be_bound_endpoint")
			}
			rel := graphQueryRelation{Kind: key, Source: ref, Direction: dir, EdgeType: typ, Target: target}
			if key == "JOIN" {
				joins++
				rel.Join = joins
			}
			plan.Relations = append(plan.Relations, rel)
			continue
		}
		if seen[key] {
			return nil, fmt.Errorf("duplicate_clause:%s", key)
		}
		seen[key] = true
		if key == "BRANCH" || key == "BRANCH_LIMIT" {
			if seen["branch_bound"] {
				return nil, fmt.Errorf("duplicate_branch_limit")
			}
			seen["branch_bound"] = true
		}
		if key == "COST" || key == "COST_LIMIT" {
			if seen["cost_bound"] {
				return nil, fmt.Errorf("duplicate_cost_limit")
			}
			seen["cost_bound"] = true
		}
		switch key {
		case "WHERE":
			where = value
		case "GROUP BY":
			group = value
		case "HOPS":
			plan.MinHops, plan.MaxHops, err = parseGraphHopBounds(value)
		case "BRANCH_LIMIT", "BRANCH":
			var n int
			n, err = strconv.Atoi(value)
			plan.BranchLimit = graphNormalizeBranchLimit(n)
		case "COST_LIMIT", "COST":
			plan.CostLimit, err = strconv.ParseFloat(value, 64)
			if plan.CostLimit < 0 || math.IsNaN(plan.CostLimit) || math.IsInf(plan.CostLimit, 0) {
				err = fmt.Errorf("invalid_cost_limit")
			}
		case "BUDGET":
			plan.Budget, err = strconv.Atoi(value)
			if plan.Budget < 1 || plan.Budget > graphQueryMaxBudget {
				err = fmt.Errorf("budget_must_be_1_to_%d", graphQueryMaxBudget)
			}
		case "CACHE":
			if !strings.EqualFold(value, "auto") && !strings.EqualFold(value, "off") {
				err = fmt.Errorf("cache_must_be_auto_or_off")
			}
			plan.CacheOff = strings.EqualFold(value, "off")
		case "RETURN":
			returns = value
		case "LIMIT":
			var n int
			n, err = strconv.Atoi(value)
			plan.Limit = graphNormalizeLimit(n)
		case "CURSOR":
			plan.Cursor, err = graphParseCursorToken(value)
		case "INCLUDE FIELDS":
			plan.IncludeFields, err = graphQueryProjectionFields(value, joins)
		case "EXCLUDE FIELDS":
			plan.ExcludeFields, err = graphQueryProjectionFields(value, joins)
		default:
			err = fmt.Errorf("unsupported_clause:%s", key)
		}
		if err != nil {
			return nil, fmt.Errorf("invalid_%s:%w", strings.ToLower(strings.ReplaceAll(key, " ", "_")), err)
		}
	}
	if returns != "" {
		if err := graphQueryParseReturn(plan, returns, joins); err != nil {
			return nil, err
		}
	}
	if where != "" {
		plan.Filter, plan.Where, err = graphQueryParseExpression(where, joins)
		if err != nil {
			return nil, err
		}
	}
	if group != "" {
		for _, name := range strings.Split(group, ",") {
			ref, err := graphQueryParseField(strings.TrimSpace(name), joins)
			if err != nil || ref.Field == "label" {
				return nil, fmt.Errorf("invalid_group_field:%s", name)
			}
			plan.GroupBy = append(plan.GroupBy, ref)
		}
		if len(plan.GroupBy) > graphQueryMaxFields {
			return nil, fmt.Errorf("too_many_group_fields")
		}
		if !seen["RETURN"] {
			plan.Return = graphReturnGroups
		}
	}
	if (len(plan.GroupBy) > 0) != (plan.Return == graphReturnGroups) {
		return nil, fmt.Errorf("group_by_requires_return_groups")
	}
	if (len(plan.GroupBy) > 0 || plan.Return == graphReturnAggregate) && len(plan.Cursor) > 0 {
		return nil, fmt.Errorf("cursor_not_supported_for_aggregation")
	}
	if plan.MaxHops > 1 && (len(plan.Relations) > 0 || len(plan.GroupBy) > 0 || plan.Return == graphReturnRows || len(plan.Select) > 0 || len(plan.IncludeFields)+len(plan.ExcludeFields) > 0) {
		return nil, fmt.Errorf("relational_clauses_require_single_hop")
	}
	if len(plan.IncludeFields)+len(plan.ExcludeFields) > 0 && plan.Return != graphReturnEdges && plan.Return != graphReturnPaths && plan.Return != graphReturnRows {
		return nil, fmt.Errorf("field_selection_requires_edges_paths_or_rows")
	}
	if len(plan.IncludeFields)+len(plan.ExcludeFields) > graphQueryMaxFields {
		return nil, fmt.Errorf("too_many_projection_fields")
	}
	return plan, nil
}

func graphQueryParsePattern(raw string) (string, string, string, graphQueryNodePattern, string, error) {
	var target graphQueryNodePattern
	left, idx, err := graphExtractGroup(raw, 0, '(', ')')
	if err != nil {
		return "", "", "", target, "", err
	}
	idx = graphSkipSpaces(raw, idx)
	dir, start := "out", 2
	if strings.HasPrefix(raw[idx:], "<-[") {
		dir, start = "in", 3
	} else if !strings.HasPrefix(raw[idx:], "-[") {
		return "", "", "", target, "", fmt.Errorf("expected_edge_pattern")
	}
	edge, next, err := graphExtractGroup(raw, idx+start-1, '[', ']')
	if err != nil {
		return "", "", "", target, "", err
	}
	typ, err := parseGraphQueryEdgePattern(edge)
	if err != nil {
		return "", "", "", target, "", err
	}
	idx = graphSkipSpaces(raw, next)
	arrow := "->"
	if dir == "in" {
		arrow = "-"
	}
	if !strings.HasPrefix(raw[idx:], arrow) {
		return "", "", "", target, "", fmt.Errorf("expected_arrow")
	}
	idx = graphSkipSpaces(raw, idx+len(arrow))
	right, idx, err := graphExtractGroup(raw, idx, '(', ')')
	if err == nil {
		target, err = parseGraphQueryNodePattern(right)
	}
	return left, dir, typ, target, strings.TrimSpace(raw[idx:]), err
}

var graphQueryClauseNames = []string{
	"INCLUDE FIELDS", "EXCLUDE FIELDS", "BRANCH_LIMIT", "COST_LIMIT", "GROUP BY",
	"INCLUDE", "EXCLUDE", "JOIN", "WHERE", "HOPS", "BRANCH", "COST", "BUDGET", "CACHE", "RETURN", "LIMIT", "CURSOR",
}

func graphQueryKeywordAt(raw string, i int, keyword string) bool {
	end := i + len(keyword)
	return end <= len(raw) && strings.EqualFold(raw[i:end], keyword) &&
		(i == 0 || graphQuerySpace(raw[i-1])) && (end == len(raw) || graphQuerySpace(raw[end]))
}

func graphQuerySpace(ch byte) bool { return ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' }

// Le parole chiave dentro stringhe o parentesi non delimitano clausole.
func graphQuerySplitClauses(raw string) ([][2]string, error) {
	var clauses [][2]string
	key, start, depth := "", 0, 0
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\'' || raw[i] == '"' {
			end, _, err := graphQueryReadString(raw, i)
			if err != nil {
				return nil, err
			}
			i = end - 1
			continue
		}
		switch raw[i] {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		}
		if depth < 0 {
			return nil, fmt.Errorf("unbalanced_query")
		}
		if depth != 0 {
			continue
		}
		for _, name := range graphQueryClauseNames {
			if !graphQueryKeywordAt(raw, i, name) {
				continue
			}
			if key == "" && strings.TrimSpace(raw[:i]) != "" {
				return nil, fmt.Errorf("unexpected_tokens_after_pattern")
			}
			if key != "" {
				clauses = append(clauses, [2]string{key, strings.TrimSpace(raw[start:i])})
			}
			key, start = name, i+len(name)
			i = start - 1
			break
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("unbalanced_query")
	}
	if key != "" {
		clauses = append(clauses, [2]string{key, strings.TrimSpace(raw[start:])})
	} else if strings.TrimSpace(raw) != "" {
		return nil, fmt.Errorf("unexpected_tokens_after_pattern")
	}
	for _, clause := range clauses {
		if clause[1] == "" {
			return nil, fmt.Errorf("clause_requires_value:%s", clause[0])
		}
	}
	return clauses, nil
}

func graphQueryProjectionFields(raw string, joins int) ([]string, error) {
	fields := strings.Split(raw, ",")
	if len(fields) > graphQueryMaxFields {
		return nil, fmt.Errorf("too_many_projection_fields")
	}
	for i := range fields {
		fields[i] = strings.TrimSpace(fields[i])
		if fields[i] == "" || len(strings.Split(fields[i], ".")) > 16 {
			return nil, fmt.Errorf("invalid_projection_field")
		}
		for _, part := range strings.Split(fields[i], ".") {
			if !graphQueryIdentifier(part) {
				return nil, fmt.Errorf("invalid_projection_field")
			}
		}
	}
	return fields, nil
}
