package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

type graphQueryField struct {
	Name     string
	Join     int
	Scope    string
	Field    string
	PropPath []string
}

type graphQueryLiteral struct {
	Value interface{}
	Kind  byte // s: stringa, n: numero, b: booleano
}

type graphQueryExpr struct {
	Op          string
	Left, Right *graphQueryExpr
	Predicate   graphQueryPredicate
}

type graphQueryToken struct {
	Text string
	Kind byte
}

func graphQueryIdentifier(raw string) bool {
	if raw == "" {
		return false
	}
	for _, ch := range raw {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_') {
			return false
		}
	}
	return true
}

func graphQueryParseField(raw string, joins int) (graphQueryField, error) {
	ref := graphQueryField{Name: raw}
	parts := strings.Split(raw, ".")
	if len(parts) > 18 {
		return ref, fmt.Errorf("field_path_too_deep")
	}
	for _, part := range parts {
		if !graphQueryIdentifier(part) {
			return ref, fmt.Errorf("invalid_field:%s", raw)
		}
	}
	if len(parts) > 0 && strings.HasPrefix(strings.ToLower(parts[0]), "join") {
		var err error
		ref.Join, err = strconv.Atoi(parts[0][4:])
		if err != nil || ref.Join < 1 || ref.Join > joins {
			return ref, fmt.Errorf("unbound_join:%s", parts[0])
		}
		parts = parts[1:]
	}
	if len(parts) < 2 {
		return ref, fmt.Errorf("expected_scope_and_field:%s", raw)
	}
	ref.Scope, ref.Field = strings.ToLower(parts[0]), strings.ToLower(parts[1])
	if ref.Scope != "edge" && ref.Scope != "from" && ref.Scope != "to" {
		return ref, fmt.Errorf("unsupported_scope:%s", ref.Scope)
	}
	if ref.Field == "props" && len(parts) > 2 {
		ref.Field, ref.PropPath = "prop", parts[2:]
		return ref, nil
	}
	if len(parts) != 2 {
		return ref, fmt.Errorf("invalid_field:%s", raw)
	}
	allowed := "id label"
	if ref.Scope == "edge" {
		allowed = "id type weight confidence modality ambiguity directed"
	}
	for _, field := range strings.Fields(allowed) {
		if ref.Field == field {
			return ref, nil
		}
	}
	return ref, fmt.Errorf("unsupported_field:%s", raw)
}

func graphQueryReadString(raw string, start int) (int, string, error) {
	quote := raw[start]
	var out strings.Builder
	for i := start + 1; i < len(raw); i++ {
		ch := raw[i]
		if ch == quote {
			if i+1 < len(raw) && raw[i+1] == quote {
				out.WriteByte(quote)
				i++
				continue
			}
			return i + 1, out.String(), nil
		}
		if ch == '\\' && i+1 < len(raw) {
			decoded, _, tail, err := strconv.UnquoteChar(raw[i:], quote)
			if err != nil {
				return 0, "", fmt.Errorf("invalid_string_escape")
			}
			consumed := len(raw[i:]) - len(tail)
			out.WriteRune(decoded)
			i += consumed - 1
			continue
		}
		out.WriteByte(ch)
	}
	return 0, "", fmt.Errorf("unterminated_string")
}

func graphQueryLex(raw string) ([]graphQueryToken, error) {
	var tokens []graphQueryToken
	for i := 0; i < len(raw); {
		if len(tokens) >= 1024 {
			return nil, fmt.Errorf("where_too_complex")
		}
		if graphQuerySpace(raw[i]) {
			i++
			continue
		}
		if raw[i] == '\'' || raw[i] == '"' {
			end, value, err := graphQueryReadString(raw, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, graphQueryToken{value, 's'})
			i = end
			continue
		}
		start := i
		if strings.ContainsRune("(),=<>!", rune(raw[i])) {
			i++
			if i < len(raw) && (raw[i] == '=' && strings.ContainsRune("<>!", rune(raw[start])) || raw[start] == '<' && raw[i] == '>') {
				i++
			}
			tokens = append(tokens, graphQueryToken{raw[start:i], 'o'})
			continue
		}
		for i < len(raw) && !graphQuerySpace(raw[i]) && !strings.ContainsRune("(),=<>!'\"", rune(raw[i])) {
			i++
		}
		if start == i {
			return nil, fmt.Errorf("invalid_where_token")
		}
		tokens = append(tokens, graphQueryToken{raw[start:i], 'w'})
	}
	return tokens, nil
}

type graphQueryExprParser struct {
	tokens []graphQueryToken
	pos    int
	joins  int
	preds  []graphQueryPredicate
}

func graphQueryParseExpression(raw string, joins int) (*graphQueryExpr, []graphQueryPredicate, error) {
	tokens, err := graphQueryLex(raw)
	if err != nil {
		return nil, nil, err
	}
	p := graphQueryExprParser{tokens: tokens, joins: joins}
	expr, err := p.parseOr(0)
	if err == nil && p.pos != len(tokens) {
		err = fmt.Errorf("unexpected_where_token:%s", tokens[p.pos].Text)
	}
	return expr, p.preds, err
}

func (p *graphQueryExprParser) take(text string) bool {
	if p.pos < len(p.tokens) && p.tokens[p.pos].Kind != 's' && strings.EqualFold(p.tokens[p.pos].Text, text) {
		p.pos++
		return true
	}
	return false
}

func (p *graphQueryExprParser) parseOr(depth int) (*graphQueryExpr, error) {
	left, err := p.parseAnd(depth)
	for err == nil && p.take("OR") {
		var right *graphQueryExpr
		right, err = p.parseAnd(depth)
		left = &graphQueryExpr{Op: "or", Left: left, Right: right}
	}
	return left, err
}

func (p *graphQueryExprParser) parseAnd(depth int) (*graphQueryExpr, error) {
	left, err := p.parseNot(depth)
	for err == nil && p.take("AND") {
		var right *graphQueryExpr
		right, err = p.parseNot(depth)
		left = &graphQueryExpr{Op: "and", Left: left, Right: right}
	}
	return left, err
}

func (p *graphQueryExprParser) parseNot(depth int) (*graphQueryExpr, error) {
	if depth >= 32 {
		return nil, fmt.Errorf("where_nesting_too_deep")
	}
	if p.take("NOT") {
		expr, err := p.parseNot(depth + 1)
		return &graphQueryExpr{Op: "not", Left: expr}, err
	}
	if p.take("(") {
		expr, err := p.parseOr(depth + 1)
		if err == nil && !p.take(")") {
			err = fmt.Errorf("missing_where_closing_parenthesis")
		}
		return expr, err
	}
	return p.parsePredicate()
}

func (p *graphQueryExprParser) parsePredicate() (*graphQueryExpr, error) {
	if p.pos >= len(p.tokens) || p.tokens[p.pos].Kind != 'w' {
		return nil, fmt.Errorf("expected_predicate_field")
	}
	field, err := graphQueryParseField(p.tokens[p.pos].Text, p.joins)
	if err != nil {
		return nil, err
	}
	p.pos++
	pred := graphQueryPredicate{graphQueryField: field}
	if p.take("IS") {
		pred.Op = "is null"
		if p.take("NOT") {
			pred.Op = "is not null"
		}
		if !p.take("NULL") {
			return nil, fmt.Errorf("expected_NULL_after_IS")
		}
	} else {
		negated := p.take("NOT")
		if p.take("IN") {
			pred.Op = "in"
			if negated {
				pred.Op = "not in"
			}
			if !p.take("(") {
				return nil, fmt.Errorf("IN_requires_list")
			}
			for {
				value, err := p.literal(field)
				if err != nil {
					return nil, err
				}
				pred.Values = append(pred.Values, value)
				if len(pred.Values) > 128 {
					return nil, fmt.Errorf("IN_list_too_large")
				}
				if p.take(")") {
					break
				}
				if !p.take(",") {
					return nil, fmt.Errorf("expected_IN_comma_or_parenthesis")
				}
			}
		} else {
			if negated || p.pos >= len(p.tokens) {
				return nil, fmt.Errorf("expected_comparison_operator")
			}
			pred.Op = p.tokens[p.pos].Text
			p.pos++
			if pred.Op == "<>" {
				pred.Op = "!="
			}
			switch pred.Op {
			case "=", "!=", "<", "<=", ">", ">=":
			default:
				return nil, fmt.Errorf("unsupported_operator:%s", pred.Op)
			}
			if p.pos < len(p.tokens) && p.tokens[p.pos].Kind == 'w' && strings.HasPrefix(p.tokens[p.pos].Text, "@") {
				ref, err := graphQueryParseField(strings.TrimPrefix(p.tokens[p.pos].Text, "@"), p.joins)
				if err != nil {
					return nil, err
				}
				if ref.Field == "label" || field.Field == "label" {
					return nil, fmt.Errorf("field_comparison_requires_scalars")
				}
				p.pos++
				pred.Reference = &ref
				p.preds = append(p.preds, pred)
				return &graphQueryExpr{Predicate: pred}, nil
			}
			value, err := p.literal(field)
			if err != nil {
				return nil, err
			}
			if pred.Op != "=" && pred.Op != "!=" && (field.Field == "label" || field.Field == "directed" || value.Kind == 'b') {
				return nil, fmt.Errorf("unordered_predicate_type")
			}
			pred.Values = []graphQueryLiteral{value}
		}
	}
	p.preds = append(p.preds, pred)
	return &graphQueryExpr{Predicate: pred}, nil
}

func (p *graphQueryExprParser) literal(field graphQueryField) (graphQueryLiteral, error) {
	var literal graphQueryLiteral
	if p.pos >= len(p.tokens) || p.tokens[p.pos].Kind == 'o' {
		return literal, fmt.Errorf("expected_predicate_literal")
	}
	token := p.tokens[p.pos]
	p.pos++
	if token.Kind != 's' && strings.HasPrefix(token.Text, "@") {
		return literal, fmt.Errorf("field_reference_requires_comparison")
	}
	literal = graphQueryLiteral{Value: token.Text, Kind: 's'}
	if token.Kind != 's' {
		if strings.EqualFold(token.Text, "null") {
			return literal, fmt.Errorf("use_IS_NULL")
		}
		if strings.EqualFold(token.Text, "true") || strings.EqualFold(token.Text, "false") {
			literal = graphQueryLiteral{Value: strings.EqualFold(token.Text, "true"), Kind: 'b'}
		} else if n, err := strconv.ParseFloat(token.Text, 64); err == nil {
			if math.IsNaN(n) || math.IsInf(n, 0) {
				return literal, fmt.Errorf("non_finite_predicate")
			}
			literal = graphQueryLiteral{Value: n, Kind: 'n'}
		}
	}
	if field.Scope == "edge" {
		switch field.Field {
		case "confidence":
			n, _, err := graphParseConfidenceToken(token.Text)
			return graphQueryLiteral{Value: n, Kind: 'n'}, err
		case "weight":
			n, err := strconv.ParseFloat(token.Text, 64)
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
				return literal, fmt.Errorf("invalid_weight_predicate")
			}
			return graphQueryLiteral{Value: n, Kind: 'n'}, nil
		case "modality":
			level, ok := graphModalityByName(token.Text)
			if !ok {
				return literal, fmt.Errorf("unknown_modality:%s", token.Text)
			}
			return graphQueryLiteral{Value: level.Name, Kind: 's'}, nil
		}
	}
	return literal, nil
}

func graphQueryCacheNode(cache map[string]*GraphNodeRecord, id string, node *GraphNodeRecord) {
	if len(cache) >= 64 {
		clear(cache)
	}
	cache[id] = node
}

func (exec *graphQueryExecution) node(id string) (*GraphNodeRecord, error) {
	if node, ok := exec.nodes[id]; ok {
		return node, nil
	}
	if err := exec.spend(1); err != nil {
		return nil, err
	}
	node, found, err := exec.db.graphGetNode(id)
	if err != nil {
		return nil, err
	}
	var result *GraphNodeRecord
	if found {
		node.References = nil
		result = &node
	}
	graphQueryCacheNode(exec.nodes, id, result)
	return result, nil
}

func (exec *graphQueryExecution) field(ref graphQueryField, row []*GraphEdgeRecord) (interface{}, bool, error) {
	if ref.Join >= len(row) || row[ref.Join] == nil {
		return nil, false, fmt.Errorf("unbound_query_field:%s", ref.Name)
	}
	edge := row[ref.Join]
	if ref.Scope == "edge" {
		switch ref.Field {
		case "id":
			return edge.ID, true, nil
		case "type":
			return edge.Type, true, nil
		case "weight":
			return edge.Weight, true, nil
		case "confidence":
			return graphEffectiveConfidence(edge), true, nil
		case "modality":
			return graphEffectiveModality(edge), true, nil
		case "ambiguity":
			return edge.Ambiguity, true, nil
		case "directed":
			return edge.Directed, true, nil
		case "prop":
			value, ok := graphLookupPropertyValue(edge.Props, ref.PropPath)
			return value, ok && value != nil, nil
		}
	}
	id := edge.From
	if ref.Scope == "to" {
		id = edge.To
	}
	if ref.Field == "id" {
		return id, true, nil
	}
	node, err := exec.node(id)
	if err != nil || node == nil {
		return nil, false, err
	}
	if ref.Field == "label" {
		return node.Labels, true, nil
	}
	value, ok := graphLookupPropertyValue(node.Props, ref.PropPath)
	return value, ok && value != nil, nil
}

func (exec *graphQueryExecution) matches(expr *graphQueryExpr, row []*GraphEdgeRecord) (bool, error) {
	if expr == nil {
		return true, nil
	}
	if expr.Op != "" {
		left, err := exec.matches(expr.Left, row)
		if err != nil {
			return false, err
		}
		switch expr.Op {
		case "not":
			return !left, nil
		case "and":
			if !left {
				return false, nil
			}
		case "or":
			if left {
				return true, nil
			}
		}
		return exec.matches(expr.Right, row)
	}
	pred := expr.Predicate
	actual, exists, err := exec.field(pred.graphQueryField, row)
	if err != nil {
		return false, err
	}
	if pred.Op == "is null" {
		return !exists, nil
	}
	if pred.Op == "is not null" {
		return exists, nil
	}
	if pred.Reference != nil {
		expected, bound, err := exec.field(*pred.Reference, row)
		if err != nil || !exists || !bound {
			return false, err
		}
		return graphQueryCompareFields(actual, pred.Op, expected)
	}
	if !exists {
		// Compatibilità: il WHERE storico considerava mancante != valore vero.
		return pred.Op == "!=", nil
	}
	if pred.Op == "in" || pred.Op == "not in" {
		found := false
		for _, expected := range pred.Values {
			if graphQueryCompare(actual, "=", expected, pred.graphQueryField) {
				found = true
				break
			}
		}
		if pred.Op == "not in" {
			found = !found
		}
		return found, nil
	}
	return graphQueryCompare(actual, pred.Op, pred.Values[0], pred.graphQueryField), nil
}

func graphQueryCompare(actual interface{}, op string, expected graphQueryLiteral, ref graphQueryField) bool {
	if labels, ok := actual.([]string); ok {
		text, ok := expected.Value.(string)
		if !ok {
			return false
		}
		contains := graphContainsLabel(labels, text)
		if op == "!=" {
			return !contains
		}
		return contains
	}
	if ref.Scope == "edge" && ref.Field == "modality" && op != "=" && op != "!=" {
		a, okA := graphModalityRank(actual.(string))
		b, okB := graphModalityRank(expected.Value.(string))
		return okA && okB && graphCompareFloat(float64(a), op, float64(b))
	}
	switch expected.Kind {
	case 'n':
		n, ok := graphValueAsFloat(actual)
		return ok && graphCompareFloat(n, op, expected.Value.(float64))
	case 'b':
		b, ok := graphValueAsBool(actual)
		if !ok {
			return false
		}
		if op == "!=" {
			return b != expected.Value.(bool)
		}
		return b == expected.Value.(bool)
	default:
		text, ok := actual.(string)
		if !ok {
			if op != "=" && op != "!=" {
				return false
			}
			text = graphPropertyValueAsString(actual)
		}
		return graphCompareString(text, op, expected.Value.(string))
	}
}
