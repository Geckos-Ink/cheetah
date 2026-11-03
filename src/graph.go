package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	graphNodePrefix      = "\x01gn:"
	graphEdgePrefix      = "\x02ge:"
	graphAdjOutPrefix    = "\x03go:"
	graphAdjInPrefix     = "\x04gi:"
	graphEdgeIndexPrefix = "graph/idx/"
	graphDefaultLimit    = 128
	graphDefaultScanPage = 256
	graphMaxLimit        = 2048
	graphMaxScanPages    = 64
	graphDefaultMinHops  = 1
	graphDefaultMaxHops  = 1
	graphMaxHops         = 16
	graphDefaultBranch   = 128
	graphMaxBranch       = 4096
	graphMaxReferences   = 64
	graphMaxReferenceLen = 4096
	graphMaxReferenceAll = 65536
)

// GraphReferenceSentence conserva una frase completa insieme alla sua
// provenienza. Le relazioni restano sugli archi; questa è l'evidenza testuale
// leggibile che un richiamo può restituire senza costringere il client a
// ricostruirla da token o label.
type GraphReferenceSentence struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Source  string `json:"source,omitempty"`
	Ordinal int    `json:"ordinal,omitempty"`
}

type GraphNodeRecord struct {
	ID         string                   `json:"id"`
	Labels     []string                 `json:"labels,omitempty"`
	Props      map[string]interface{}   `json:"props,omitempty"`
	References []GraphReferenceSentence `json:"references,omitempty"`
	CreatedAt  string                   `json:"created_at,omitempty"`
	UpdatedAt  string                   `json:"updated_at,omitempty"`
}

type GraphEdgeRecord struct {
	ID   string `json:"id"`
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type,omitempty"`
	// Directed/Weight descrivono la topologia; Confidence/Modality/Ambiguity
	// descrivono quanto ci si crede (vedi graph_uncertainty.go). Confidence è un
	// puntatore perché 0 è un valore legittimo (`ruled_out`) e va distinto da
	// "non dichiarato", che vale `certain`.
	Directed   bool                   `json:"directed"`
	Weight     float64                `json:"weight"`
	Confidence *float64               `json:"confidence,omitempty"`
	Modality   string                 `json:"modality,omitempty"`
	Ambiguity  string                 `json:"ambiguity,omitempty"`
	Props      map[string]interface{} `json:"props,omitempty"`
	CreatedAt  string                 `json:"created_at,omitempty"`
	UpdatedAt  string                 `json:"updated_at,omitempty"`
}

type graphQueryReturnMode string

const (
	graphReturnEdges     graphQueryReturnMode = "edges"
	graphReturnNodes     graphQueryReturnMode = "nodes"
	graphReturnPaths     graphQueryReturnMode = "paths"
	graphReturnCount     graphQueryReturnMode = "count"
	graphReturnRows      graphQueryReturnMode = "rows"
	graphReturnGroups    graphQueryReturnMode = "groups"
	graphReturnValues    graphQueryReturnMode = "values"
	graphReturnAggregate graphQueryReturnMode = "aggregate"
)

type graphQueryNodePattern struct {
	Wildcard bool
	ID       string
	Label    string
}

type graphQueryPredicate struct {
	graphQueryField
	Op        string
	Values    []graphQueryLiteral
	Reference *graphQueryField
}

type graphQueryPlan struct {
	Direction     string
	EdgeType      string
	Left          graphQueryNodePattern
	Right         graphQueryNodePattern
	Where         []graphQueryPredicate
	MinHops       int
	MaxHops       int
	BranchLimit   int
	CostLimit     float64
	Return        graphQueryReturnMode
	Limit         int
	Cursor        []byte
	Filter        *graphQueryExpr
	Relations     []graphQueryRelation
	GroupBy       []graphQueryField
	IncludeFields []string
	ExcludeFields []string
	Budget        int
	CacheOff      bool
	Select        []graphQuerySelect
	Distinct      bool
}

type graphPathView struct {
	From   string  `json:"from"`
	Type   string  `json:"type,omitempty"`
	To     string  `json:"to"`
	Weight float64 `json:"weight"`
}

type graphRelationTypeCount struct {
	Type     string  `json:"type"`
	Count    int     `json:"count"`
	Weighted float64 `json:"weighted,omitempty"`
}

type graphEdgeSetRequest struct {
	From            string
	To              string
	Type            string
	Directed        bool
	Weight          float64
	Props           map[string]interface{}
	AutoCreateNodes bool
	// SetUncertainty/SetAmbiguity distinguono "non passato" (si conserva il valore
	// memorizzato) da "passato esplicitamente" (si sovrascrive, anche azzerando).
	Confidence     *float64
	Modality       string
	Ambiguity      string
	SetUncertainty bool
	SetAmbiguity   bool
}

type graphEdgeSetBatchItem struct {
	From       string                 `json:"from"`
	To         string                 `json:"to"`
	Type       string                 `json:"type,omitempty"`
	Directed   *bool                  `json:"directed,omitempty"`
	Weight     *float64               `json:"weight,omitempty"`
	Confidence *json.RawMessage       `json:"confidence,omitempty"`
	Modality   string                 `json:"modality,omitempty"`
	Ambiguity  string                 `json:"ambiguity,omitempty"`
	Props      map[string]interface{} `json:"props,omitempty"`
	AutoCreate *bool                  `json:"autocreate,omitempty"`
}

// graphBatchConfidenceToken riporta `"confidence"` a un token testuale: nel JSON
// di batch può arrivare sia come numero (0.4) sia come parola ("possible").
func graphBatchConfidenceToken(raw *json.RawMessage) (string, error) {
	if raw == nil {
		return "", nil
	}
	trimmed := strings.TrimSpace(string(*raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var text string
		if err := json.Unmarshal(*raw, &text); err != nil {
			return "", err
		}
		return strings.TrimSpace(text), nil
	}
	return trimmed, nil
}

type graphBatchError struct {
	Index int    `json:"index"`
	Error string `json:"error"`
}

func (db *Database) handleGraphNodeSet(args string) (string, error) {
	params := parseKeyValueArgs(args)
	id := graphNormalizeID(params["id"])
	if id == "" {
		return "ERROR,graph_node_set_requires_id", nil
	}
	labels := graphParseLabels(params["labels"])
	props, err := graphParseProps(params["props"])
	if err != nil {
		return fmt.Sprintf("ERROR,invalid_props:%v", err), nil
	}
	references, referencesSet, err := graphParseReferences(params["references"])
	if err != nil {
		return fmt.Sprintf("ERROR,invalid_references:%v", err), nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	existing, found, err := db.graphGetNode(id)
	if err != nil {
		return "", err
	}
	record := GraphNodeRecord{
		ID:         id,
		Labels:     labels,
		Props:      props,
		References: references,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if found {
		record.CreatedAt = existing.CreatedAt
		if len(labels) == 0 {
			record.Labels = existing.Labels
		}
		if props == nil {
			record.Props = existing.Props
		}
		if !referencesSet {
			record.References = existing.References
		}
	}
	if err := db.graphPutNode(record); err != nil {
		return "", err
	}
	var previous *GraphNodeRecord
	if found {
		previous = &existing
	}
	if err := db.graphSyncNodeTerms(previous, &record); err != nil {
		return "", err
	}
	return fmt.Sprintf("SUCCESS,node_set,id=%s", id), nil
}

func (db *Database) handleGraphNodeGet(args string) (string, error) {
	params := parseKeyValueArgs(args)
	id := graphNormalizeID(params["id"])
	if id == "" {
		return "ERROR,graph_node_get_requires_id", nil
	}
	record, found, err := db.graphGetNode(id)
	if err != nil {
		return "", err
	}
	if !found {
		return "ERROR,node_not_found", nil
	}
	payload, err := graphEncodeJSON(record)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("SUCCESS,id=%s,payload=%s", id, payload), nil
}

func (db *Database) handleGraphEdgeSet(args string) (string, error) {
	params := parseKeyValueArgs(args)
	request, errResp, err := graphBuildEdgeSetRequestFromParams(params)
	if errResp != "" {
		return errResp, nil
	}
	if err != nil {
		return "", err
	}
	edgeID, _, err := db.graphUpsertEdge(request)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("SUCCESS,edge_set,id=%s", edgeID), nil
}

func (db *Database) handleGraphEdgeSetBatch(args string) (string, error) {
	params := parseKeyValueArgs(args)
	rawItems := strings.TrimSpace(params["items"])
	if rawItems == "" {
		rawItems = strings.TrimSpace(params["json"])
	}
	if rawItems == "" {
		return "ERROR,graph_edge_set_batch_requires_items", nil
	}

	defaultReq := graphEdgeSetRequest{
		Type:            graphNormalizeEdgeType(params["type"]),
		Directed:        true,
		Weight:          1.0,
		AutoCreateNodes: true,
	}
	if raw := strings.TrimSpace(params["directed"]); raw != "" {
		defaultReq.Directed = parseBoolFlag(raw)
	}
	if raw := strings.TrimSpace(params["weight"]); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return "ERROR,invalid_weight", nil
		}
		defaultReq.Weight = parsed
	}
	if props, err := graphParseProps(params["props"]); err != nil {
		return fmt.Sprintf("ERROR,invalid_props:%v", err), nil
	} else {
		defaultReq.Props = props
	}
	if raw := strings.TrimSpace(params["autocreate"]); raw != "" {
		defaultReq.AutoCreateNodes = parseBoolFlag(raw)
	}
	if raw := strings.TrimSpace(params["ensure_nodes"]); raw != "" {
		defaultReq.AutoCreateNodes = parseBoolFlag(raw)
	}
	if confidence, modality, setUncertainty, err := graphResolveUncertaintyArgs(params["confidence"], params["modality"]); err != nil {
		return fmt.Sprintf("ERROR,invalid_confidence:%v", err), nil
	} else if setUncertainty {
		defaultReq.Confidence = confidence
		defaultReq.Modality = modality
		defaultReq.SetUncertainty = true
	}
	if ambiguity := strings.TrimSpace(params["ambiguity"]); ambiguity != "" {
		defaultReq.SetAmbiguity = true
		defaultReq.Ambiguity = ambiguity
		if ambiguity == graphClearToken {
			defaultReq.Ambiguity = ""
		}
	}

	continueOnError := parseBoolFlag(params["continue_on_error"]) || parseBoolFlag(params["continueonerror"])

	data, err := graphDecodeMaybeBase64JSON(rawItems)
	if err != nil {
		return fmt.Sprintf("ERROR,invalid_items:%v", err), nil
	}
	var items []graphEdgeSetBatchItem
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Sprintf("ERROR,invalid_items:%v", err), nil
	}
	if len(items) == 0 {
		return "ERROR,graph_edge_set_batch_requires_nonempty_items", nil
	}

	applied := 0
	created := 0
	updated := 0
	batchErrs := make([]graphBatchError, 0)
	for idx, item := range items {
		req := defaultReq
		if from := graphNormalizeID(item.From); from != "" {
			req.From = from
		}
		if to := graphNormalizeID(item.To); to != "" {
			req.To = to
		}
		if typ := graphNormalizeEdgeType(item.Type); typ != "" {
			req.Type = typ
		}
		if item.Directed != nil {
			req.Directed = *item.Directed
		}
		if item.Weight != nil {
			req.Weight = *item.Weight
		}
		if item.Props != nil {
			req.Props = item.Props
		}
		if item.AutoCreate != nil {
			req.AutoCreateNodes = *item.AutoCreate
		}
		confidenceToken, tokenErr := graphBatchConfidenceToken(item.Confidence)
		if tokenErr != nil {
			batchErrs = append(batchErrs, graphBatchError{Index: idx, Error: fmt.Sprintf("invalid_confidence:%v", tokenErr)})
			if !continueOnError {
				payload, _ := graphEncodeJSON(batchErrs)
				return fmt.Sprintf("ERROR,graph_edge_set_batch_failed,applied=%d,payload=%s", applied, payload), nil
			}
			continue
		}
		confidence, modality, setUncertainty, uncertaintyErr := graphResolveUncertaintyArgs(confidenceToken, item.Modality)
		if uncertaintyErr != nil {
			batchErrs = append(batchErrs, graphBatchError{Index: idx, Error: fmt.Sprintf("invalid_confidence:%v", uncertaintyErr)})
			if !continueOnError {
				payload, _ := graphEncodeJSON(batchErrs)
				return fmt.Sprintf("ERROR,graph_edge_set_batch_failed,applied=%d,payload=%s", applied, payload), nil
			}
			continue
		}
		if setUncertainty {
			req.Confidence = confidence
			req.Modality = modality
			req.SetUncertainty = true
		}
		if ambiguity := strings.TrimSpace(item.Ambiguity); ambiguity != "" {
			req.SetAmbiguity = true
			req.Ambiguity = ambiguity
			if ambiguity == graphClearToken {
				req.Ambiguity = ""
			}
		}
		_, existed, err := db.graphUpsertEdge(req)
		if err != nil {
			batchErrs = append(batchErrs, graphBatchError{Index: idx, Error: err.Error()})
			if !continueOnError {
				payload, _ := graphEncodeJSON(batchErrs)
				return fmt.Sprintf("ERROR,graph_edge_set_batch_failed,applied=%d,payload=%s", applied, payload), nil
			}
			continue
		}
		applied++
		if existed {
			updated++
		} else {
			created++
		}
	}
	if len(batchErrs) > 0 {
		payload, _ := graphEncodeJSON(batchErrs)
		return fmt.Sprintf(
			"SUCCESS,requested=%d,applied=%d,created=%d,updated=%d,failed=%d,payload=%s",
			len(items),
			applied,
			created,
			updated,
			len(batchErrs),
			payload,
		), nil
	}
	return fmt.Sprintf(
		"SUCCESS,requested=%d,applied=%d,created=%d,updated=%d,failed=0",
		len(items),
		applied,
		created,
		updated,
	), nil
}

func graphBuildEdgeSetRequestFromParams(params map[string]string) (graphEdgeSetRequest, string, error) {
	fromID := graphNormalizeID(params["from"])
	toID := graphNormalizeID(params["to"])
	if fromID == "" || toID == "" {
		return graphEdgeSetRequest{}, "ERROR,graph_edge_set_requires_from_and_to", nil
	}
	edgeType := graphNormalizeEdgeType(params["type"])
	directed := true
	if raw := strings.TrimSpace(params["directed"]); raw != "" {
		directed = parseBoolFlag(raw)
	}
	weight := 1.0
	if raw := strings.TrimSpace(params["weight"]); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return graphEdgeSetRequest{}, "ERROR,invalid_weight", nil
		}
		weight = parsed
	}
	props, err := graphParseProps(params["props"])
	if err != nil {
		return graphEdgeSetRequest{}, fmt.Sprintf("ERROR,invalid_props:%v", err), nil
	}
	autoCreateNodes := true
	if raw := strings.TrimSpace(params["autocreate"]); raw != "" {
		autoCreateNodes = parseBoolFlag(raw)
	}
	if raw := strings.TrimSpace(params["ensure_nodes"]); raw != "" {
		autoCreateNodes = parseBoolFlag(raw)
	}
	confidence, modality, setUncertainty, err := graphResolveUncertaintyArgs(params["confidence"], params["modality"])
	if err != nil {
		return graphEdgeSetRequest{}, fmt.Sprintf("ERROR,invalid_confidence:%v", err), nil
	}
	ambiguity := strings.TrimSpace(params["ambiguity"])
	if ambiguity == "" {
		ambiguity = strings.TrimSpace(params["oneof"])
	}
	setAmbiguity := ambiguity != ""
	if ambiguity == graphClearToken {
		ambiguity = ""
	}
	return graphEdgeSetRequest{
		From:            fromID,
		To:              toID,
		Type:            edgeType,
		Directed:        directed,
		Weight:          weight,
		Props:           props,
		AutoCreateNodes: autoCreateNodes,
		Confidence:      confidence,
		Modality:        modality,
		Ambiguity:       ambiguity,
		SetUncertainty:  setUncertainty,
		SetAmbiguity:    setAmbiguity,
	}, "", nil
}

func graphDecodeMaybeBase64JSON(raw string) ([]byte, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("empty_payload")
	}
	if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") {
		return []byte(trimmed), nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil {
		return decoded, nil
	}
	decoded, err := base64.RawStdEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

func (db *Database) graphUpsertEdge(request graphEdgeSetRequest) (string, bool, error) {
	fromID := graphNormalizeID(request.From)
	toID := graphNormalizeID(request.To)
	if fromID == "" || toID == "" {
		return "", false, fmt.Errorf("graph_edge_set_requires_from_and_to")
	}
	edgeType := graphNormalizeEdgeType(request.Type)
	directed := request.Directed
	weight := request.Weight
	if math.IsNaN(weight) || math.IsInf(weight, 0) {
		weight = 1.0
	}

	if request.AutoCreateNodes {
		if err := db.graphEnsureNode(fromID); err != nil {
			return "", false, err
		}
		if err := db.graphEnsureNode(toID); err != nil {
			return "", false, err
		}
	} else {
		if _, ok, err := db.graphGetNode(fromID); err != nil {
			return "", false, err
		} else if !ok {
			return "", false, fmt.Errorf("from_node_not_found")
		}
		if _, ok, err := db.graphGetNode(toID); err != nil {
			return "", false, err
		} else if !ok {
			return "", false, fmt.Errorf("to_node_not_found")
		}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	edgeID := graphEdgeID(fromID, toID, edgeType, directed)
	record := GraphEdgeRecord{
		ID:         edgeID,
		From:       fromID,
		To:         toID,
		Type:       edgeType,
		Directed:   directed,
		Weight:     weight,
		Confidence: request.Confidence,
		Modality:   request.Modality,
		Ambiguity:  request.Ambiguity,
		Props:      request.Props,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	existed := false
	var existingRecord GraphEdgeRecord
	if existing, found, err := db.graphGetEdge(fromID, toID, edgeType, directed); err != nil {
		return "", false, err
	} else if found {
		existed = true
		existingRecord = existing
		record.CreatedAt = existing.CreatedAt
		if request.Props == nil {
			record.Props = existing.Props
		}
		// Una credenza si conserva finché non la si ridichiara: a differenza di
		// weight, omettere confidence/modality/ambiguity non le azzera.
		if !request.SetUncertainty {
			record.Confidence = existing.Confidence
			record.Modality = existing.Modality
		}
		if !request.SetAmbiguity {
			record.Ambiguity = existing.Ambiguity
		}
	}
	if record.Confidence != nil && record.Modality == "" {
		record.Modality = graphModalityForConfidence(*record.Confidence)
	}
	if err := db.graphPutEdge(record); err != nil {
		return "", false, err
	}
	if existed {
		if err := db.graphDeleteEdgePropertyIndexes(existingRecord); err != nil {
			return "", false, err
		}
	}
	if err := db.graphPutEdgePropertyIndexes(record); err != nil {
		return "", false, err
	}
	return edgeID, existed, nil
}

func (db *Database) handleGraphEdgeGet(args string) (string, error) {
	params := parseKeyValueArgs(args)
	fromID := graphNormalizeID(params["from"])
	toID := graphNormalizeID(params["to"])
	if fromID == "" || toID == "" {
		return "ERROR,graph_edge_get_requires_from_and_to", nil
	}
	edgeType := graphNormalizeEdgeType(params["type"])
	directed := true
	if raw := strings.TrimSpace(params["directed"]); raw != "" {
		directed = parseBoolFlag(raw)
	}
	record, found, err := db.graphGetEdge(fromID, toID, edgeType, directed)
	if err != nil {
		return "", err
	}
	if !found {
		return "ERROR,edge_not_found", nil
	}
	payload, err := graphEncodeJSON(record)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("SUCCESS,id=%s,payload=%s", record.ID, payload), nil
}

func (db *Database) handleGraphNeighbors(args string) (string, error) {
	params := parseKeyValueArgs(args)
	nodeID := graphNormalizeID(params["id"])
	if nodeID == "" {
		return "ERROR,graph_neighbors_requires_id", nil
	}
	direction := strings.ToLower(strings.TrimSpace(params["direction"]))
	if direction == "" {
		direction = "out"
	}
	edgeType := graphNormalizeEdgeType(params["type"])
	if edgeType == "*" {
		edgeType = ""
	}
	limit := graphDefaultLimit
	if raw := strings.TrimSpace(params["limit"]); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return "ERROR,invalid_limit", nil
		}
		limit = parsed
	}
	limit = graphNormalizeLimit(limit)
	cursor, err := graphParseCursorToken(params["cursor"])
	if err != nil {
		return fmt.Sprintf("ERROR,invalid_cursor:%v", err), nil
	}

	switch direction {
	case "out":
		prefix := graphAdjOutScanPrefix(nodeID, edgeType)
		edges, nextCursor, err := db.graphScanAdjacency(prefix, limit, cursor, nil)
		if err != nil {
			return "", err
		}
		return graphFormatEdgesResponse(edges, nextCursor)
	case "in":
		prefix := graphAdjInScanPrefix(nodeID, edgeType)
		edges, nextCursor, err := db.graphScanAdjacency(prefix, limit, cursor, nil)
		if err != nil {
			return "", err
		}
		return graphFormatEdgesResponse(edges, nextCursor)
	case "both":
		if len(cursor) > 0 {
			return "ERROR,cursor_not_supported_with_direction_both", nil
		}
		outEdges, _, err := db.graphScanAdjacency(graphAdjOutScanPrefix(nodeID, edgeType), limit, nil, nil)
		if err != nil {
			return "", err
		}
		inEdges, _, err := db.graphScanAdjacency(graphAdjInScanPrefix(nodeID, edgeType), limit, nil, nil)
		if err != nil {
			return "", err
		}
		merged := make([]GraphEdgeRecord, 0, len(outEdges)+len(inEdges))
		seen := make(map[string]struct{}, len(outEdges)+len(inEdges))
		for _, edge := range outEdges {
			if _, ok := seen[edge.ID]; ok {
				continue
			}
			seen[edge.ID] = struct{}{}
			merged = append(merged, edge)
		}
		for _, edge := range inEdges {
			if _, ok := seen[edge.ID]; ok {
				continue
			}
			seen[edge.ID] = struct{}{}
			merged = append(merged, edge)
		}
		sort.Slice(merged, func(i, j int) bool {
			if merged[i].From == merged[j].From {
				if merged[i].Type == merged[j].Type {
					return merged[i].To < merged[j].To
				}
				return merged[i].Type < merged[j].Type
			}
			return merged[i].From < merged[j].From
		})
		if len(merged) > limit {
			merged = merged[:limit]
		}
		return graphFormatEdgesResponse(merged, nil)
	default:
		return "ERROR,invalid_direction", nil
	}
}

func (db *Database) handleGraphDegree(args string) (string, error) {
	params := parseKeyValueArgs(args)
	nodeID := graphNormalizeID(params["id"])
	if nodeID == "" {
		return "ERROR,graph_degree_requires_id", nil
	}
	direction := strings.ToLower(strings.TrimSpace(params["direction"]))
	if direction == "" {
		direction = "out"
	}
	edgeType := graphNormalizeEdgeType(params["type"])
	if edgeType == "*" {
		edgeType = ""
	}
	weighted := parseBoolFlag(params["weighted"])

	totalCount := 0
	totalWeight := 0.0
	collect := func(prefix []byte) error {
		count, weight, err := db.graphCountAdjacencyPrefix(prefix, weighted)
		if err != nil {
			return err
		}
		totalCount += count
		totalWeight += weight
		return nil
	}
	switch direction {
	case "out":
		if err := collect(graphAdjOutScanPrefix(nodeID, edgeType)); err != nil {
			return "", err
		}
	case "in":
		if err := collect(graphAdjInScanPrefix(nodeID, edgeType)); err != nil {
			return "", err
		}
	case "both":
		if err := collect(graphAdjOutScanPrefix(nodeID, edgeType)); err != nil {
			return "", err
		}
		if err := collect(graphAdjInScanPrefix(nodeID, edgeType)); err != nil {
			return "", err
		}
	default:
		return "ERROR,invalid_direction", nil
	}
	if weighted {
		return fmt.Sprintf(
			"SUCCESS,id=%s,direction=%s,type=%s,degree=%d,weighted_degree=%.6f",
			nodeID,
			direction,
			graphDegreeTypeLabel(edgeType),
			totalCount,
			totalWeight,
		), nil
	}
	return fmt.Sprintf(
		"SUCCESS,id=%s,direction=%s,type=%s,degree=%d",
		nodeID,
		direction,
		graphDegreeTypeLabel(edgeType),
		totalCount,
	), nil
}

func graphDegreeTypeLabel(edgeType string) string {
	if edgeType == "" {
		return "*"
	}
	return edgeType
}

func (db *Database) handleGraphNeighborTypes(args string) (string, error) {
	params := parseKeyValueArgs(args)
	nodeID := graphNormalizeID(params["id"])
	if nodeID == "" {
		return "ERROR,graph_neighbor_types_requires_id", nil
	}
	direction := strings.ToLower(strings.TrimSpace(params["direction"]))
	if direction == "" {
		direction = "out"
	}
	limit := graphDefaultLimit
	if raw := strings.TrimSpace(params["limit"]); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return "ERROR,invalid_limit", nil
		}
		limit = parsed
	}
	limit = graphNormalizeLimit(limit)
	cursor, err := graphParseCursorToken(params["cursor"])
	if err != nil {
		return fmt.Sprintf("ERROR,invalid_cursor:%v", err), nil
	}
	weighted := parseBoolFlag(params["weighted"])

	switch direction {
	case "out":
		prefix := graphAdjOutScanPrefix(nodeID, "")
		counts, nextCursor, err := db.graphScanNeighborTypes(prefix, limit, cursor, "out", weighted)
		if err != nil {
			return "", err
		}
		return graphFormatNeighborTypesResponse(counts, nextCursor)
	case "in":
		prefix := graphAdjInScanPrefix(nodeID, "")
		counts, nextCursor, err := db.graphScanNeighborTypes(prefix, limit, cursor, "in", weighted)
		if err != nil {
			return "", err
		}
		return graphFormatNeighborTypesResponse(counts, nextCursor)
	case "both":
		if len(cursor) > 0 {
			return "ERROR,cursor_not_supported_with_direction_both", nil
		}
		outCounts, _, err := db.graphScanNeighborTypes(graphAdjOutScanPrefix(nodeID, ""), limit, nil, "out", weighted)
		if err != nil {
			return "", err
		}
		inCounts, _, err := db.graphScanNeighborTypes(graphAdjInScanPrefix(nodeID, ""), limit, nil, "in", weighted)
		if err != nil {
			return "", err
		}
		merged := mergeGraphNeighborTypeCounts(outCounts, inCounts)
		return graphFormatNeighborTypesResponse(merged, nil)
	default:
		return "ERROR,invalid_direction", nil
	}
}

func (db *Database) handleGraphQuery(args string) (string, error) {
	query := strings.TrimSpace(args)
	if query == "" {
		return "ERROR,graph_query_requires_syntax", nil
	}
	plan, err := parseGraphQuery(query)
	if err != nil {
		return "ERROR,graph_query_parse_failed:" + strings.NewReplacer("\r", "\\r", "\n", "\\n").Replace(err.Error()), nil
	}
	if plan.MaxHops == 1 {
		return db.executeGraphRelationalQuery(plan)
	}
	edges, nextCursor, err := db.executeGraphQuery(plan)
	if err != nil {
		return "", err
	}
	switch plan.Return {
	case graphReturnCount:
		return fmt.Sprintf("SUCCESS,return=count,matches=%d,next_cursor=%s", len(edges), graphCursorToken(nextCursor)), nil
	case graphReturnNodes:
		unique := make(map[string]struct{}, len(edges)*2)
		for _, edge := range edges {
			unique[edge.From] = struct{}{}
			unique[edge.To] = struct{}{}
		}
		nodes := make([]string, 0, len(unique))
		for id := range unique {
			nodes = append(nodes, id)
		}
		sort.Strings(nodes)
		payload, err := graphEncodeJSON(nodes)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("SUCCESS,return=nodes,matches=%d,next_cursor=%s,payload=%s", len(nodes), graphCursorToken(nextCursor), payload), nil
	case graphReturnPaths:
		paths := make([]graphPathView, 0, len(edges))
		for _, edge := range edges {
			paths = append(paths, graphPathView{
				From:   edge.From,
				Type:   edge.Type,
				To:     edge.To,
				Weight: edge.Weight,
			})
		}
		payload, err := graphEncodeJSON(paths)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("SUCCESS,return=paths,matches=%d,next_cursor=%s,payload=%s", len(paths), graphCursorToken(nextCursor), payload), nil
	default:
		payload, err := graphEncodeJSON(edges)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("SUCCESS,return=edges,matches=%d,next_cursor=%s,payload=%s", len(edges), graphCursorToken(nextCursor), payload), nil
	}
}

func (db *Database) executeGraphQuery(plan *graphQueryPlan) ([]GraphEdgeRecord, []byte, error) {
	if plan == nil {
		return nil, nil, fmt.Errorf("nil plan")
	}
	if plan.MinHops < 1 {
		plan.MinHops = graphDefaultMinHops
	}
	if plan.MaxHops < plan.MinHops {
		plan.MaxHops = plan.MinHops
	}
	if plan.MaxHops > graphMaxHops {
		plan.MaxHops = graphMaxHops
	}
	if plan.BranchLimit <= 0 {
		plan.BranchLimit = graphDefaultBranch
	}
	if plan.BranchLimit > graphMaxBranch {
		plan.BranchLimit = graphMaxBranch
	}
	if plan.MaxHops > 1 && len(plan.Cursor) > 0 {
		return nil, nil, fmt.Errorf("cursor_not_supported_for_multihop")
	}
	if plan.MaxHops <= 1 {
		return nil, nil, fmt.Errorf("single_hop_uses_streaming_executor")
	}
	return db.executeGraphQueryMultiHop(plan)
}

type graphTraversalState struct {
	Node    string
	Hops    int
	Cost    float64
	Visited map[string]struct{}
}

type graphTraversalCandidate struct {
	Weight   float64
	NextNode string
	NextCost float64
}

func (db *Database) executeGraphQueryMultiHop(plan *graphQueryPlan) ([]GraphEdgeRecord, []byte, error) {
	limit := graphNormalizeLimit(plan.Limit)
	if limit <= 0 {
		limit = graphDefaultLimit
	}
	exec := newGraphQueryExecution(db, plan)
	ok, err := exec.matchNode(plan.Left.ID, plan.Left)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, nil
	}
	results := make([]GraphEdgeRecord, 0, limit)
	outputBytes := 0
	seenTerminal := make(map[string]struct{}, limit*2)
	queue := make([]graphTraversalState, 0, graphDefaultLimit)
	queue = append(queue, graphTraversalState{
		Node:    plan.Left.ID,
		Hops:    0,
		Cost:    0,
		Visited: map[string]struct{}{plan.Left.ID: {}},
	})

	for len(queue) > 0 && len(results) < limit {
		state := queue[0]
		queue[0] = graphTraversalState{}
		queue = queue[1:]
		if state.Hops >= plan.MaxHops {
			continue
		}

		adjPrefix := graphAdjOutScanPrefix(state.Node, plan.EdgeType)
		if plan.Direction == "in" {
			adjPrefix = graphAdjInScanPrefix(state.Node, plan.EdgeType)
		}
		branchFetch := plan.BranchLimit
		if branchFetch < graphDefaultScanPage {
			branchFetch = graphDefaultScanPage
		}
		candidates := make([]graphTraversalCandidate, 0, plan.BranchLimit)
		consumed := 0
		_, err := exec.scan(adjPrefix, nil, func(edge GraphEdgeRecord, _ []byte) (bool, error) {
			consumed++
			more := consumed < branchFetch
			nextNode := edge.To
			if plan.Direction == "in" {
				nextNode = edge.From
			}
			nextHops := state.Hops + 1
			stepCost := graphEdgeTraversalCost(edge.Weight)
			if math.IsInf(stepCost, 1) {
				return more, nil
			}
			nextCost := state.Cost + stepCost
			if plan.CostLimit > 0 && nextCost > plan.CostLimit {
				return more, nil
			}
			ok, err := exec.matches(plan.Filter, []*GraphEdgeRecord{&edge})
			if err != nil || !ok {
				return more, err
			}
			if nextHops >= plan.MinHops {
				ok, err := exec.matchNode(nextNode, plan.Right)
				if err != nil {
					return false, err
				}
				if ok {
					terminalKey := fmt.Sprintf("%s|%d", edge.ID, nextHops)
					if _, exists := seenTerminal[terminalKey]; !exists {
						seenTerminal[terminalKey] = struct{}{}
						retained := edge
						if plan.Return != graphReturnEdges {
							retained = GraphEdgeRecord{From: edge.From, To: edge.To, Type: edge.Type, Weight: edge.Weight}
						}
						raw, err := json.Marshal(retained)
						if err != nil {
							return false, err
						}
						outputBytes += len(raw)
						if outputBytes > graphQueryMaxOutputBytes {
							return false, fmt.Errorf("graph_query_output_capacity_exceeded")
						}
						results = append(results, retained)
						if len(results) >= limit {
							return false, nil
						}
					}
				}
			}
			if nextHops < plan.MaxHops {
				if _, seen := state.Visited[nextNode]; !seen {
					candidates = append(candidates, graphTraversalCandidate{Weight: edge.Weight, NextNode: nextNode, NextCost: nextCost})
				}
			}
			return more, nil
		})
		if err != nil {
			return nil, nil, err
		}
		if len(results) >= limit {
			break
		}

		if len(candidates) == 0 {
			continue
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].Weight == candidates[j].Weight {
				if candidates[i].NextCost == candidates[j].NextCost {
					return candidates[i].NextNode < candidates[j].NextNode
				}
				return candidates[i].NextCost < candidates[j].NextCost
			}
			return candidates[i].Weight > candidates[j].Weight
		})
		if len(candidates) > plan.BranchLimit {
			candidates = candidates[:plan.BranchLimit]
		}
		for _, cand := range candidates {
			if len(queue) >= graphQueryMaxFrontier {
				return nil, nil, fmt.Errorf("graph_query_frontier_exceeded")
			}
			nextVisited := make(map[string]struct{}, len(state.Visited)+1)
			for key := range state.Visited {
				nextVisited[key] = struct{}{}
			}
			nextVisited[cand.NextNode] = struct{}{}
			queue = append(queue, graphTraversalState{
				Node:    cand.NextNode,
				Hops:    state.Hops + 1,
				Cost:    cand.NextCost,
				Visited: nextVisited,
			})
		}
	}
	return results, nil, nil
}

func parseGraphQueryNodePattern(raw string) (graphQueryNodePattern, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return graphQueryNodePattern{}, fmt.Errorf("empty_node_pattern")
	}
	if trimmed == "*" {
		return graphQueryNodePattern{Wildcard: true}, nil
	}
	pattern := graphQueryNodePattern{}
	if !strings.Contains(trimmed, "=") {
		id := graphNormalizeID(graphUnquote(trimmed))
		if id == "" {
			return pattern, fmt.Errorf("invalid_id")
		}
		pattern.ID = id
		return pattern, nil
	}
	fields, err := graphSplitCSV(trimmed)
	if err != nil {
		return pattern, err
	}
	for _, field := range fields {
		key, val, ok := strings.Cut(field, "=")
		if !ok {
			return pattern, fmt.Errorf("invalid_node_assignment")
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = graphUnquote(strings.TrimSpace(val))
		switch key {
		case "id":
			pattern.ID = graphNormalizeID(val)
		case "label":
			pattern.Label = strings.TrimSpace(val)
		default:
			return pattern, fmt.Errorf("unsupported_node_field:%s", key)
		}
	}
	if pattern.ID == "" && pattern.Label == "" {
		return pattern, fmt.Errorf("node_id_or_label_required")
	}
	return pattern, nil
}

func parseGraphQueryEdgePattern(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	if !strings.HasPrefix(trimmed, ":") {
		return "", fmt.Errorf("edge_pattern_must_use_colon_type")
	}
	value := strings.TrimSpace(trimmed[1:])
	value = graphUnquote(value)
	if value == "*" {
		return "", nil
	}
	return graphNormalizeEdgeType(value), nil
}

func parseGraphReturnMode(raw string) (graphQueryReturnMode, error) {
	mode := strings.ToLower(strings.TrimSpace(raw))
	switch mode {
	case "edges":
		return graphReturnEdges, nil
	case "nodes":
		return graphReturnNodes, nil
	case "paths":
		return graphReturnPaths, nil
	case "count":
		return graphReturnCount, nil
	case "rows":
		return graphReturnRows, nil
	case "groups":
		return graphReturnGroups, nil
	default:
		return "", fmt.Errorf("invalid_return_mode")
	}
}

func graphSkipSpaces(raw string, idx int) int {
	for idx < len(raw) {
		if raw[idx] != ' ' && raw[idx] != '\t' && raw[idx] != '\n' && raw[idx] != '\r' {
			break
		}
		idx++
	}
	return idx
}

func graphExtractGroup(raw string, start int, open byte, close byte) (string, int, error) {
	if start >= len(raw) || raw[start] != open {
		return "", start, fmt.Errorf("expected_%c", open)
	}
	depth := 0
	for i := start; i < len(raw); i++ {
		if raw[i] == '\'' || raw[i] == '"' {
			end, _, err := graphQueryReadString(raw, i)
			if err != nil {
				return "", start, err
			}
			i = end - 1
			continue
		}
		if raw[i] == open {
			depth++
		}
		if raw[i] == close {
			depth--
			if depth == 0 {
				return raw[start+1 : i], i + 1, nil
			}
		}
	}
	return "", start, fmt.Errorf("unterminated_group")
}

func parseGraphHopBounds(raw string) (int, int, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, 0, fmt.Errorf("hops_clause_requires_value")
	}
	normalize := func(minHops int, maxHops int) (int, int, error) {
		if minHops < 1 || maxHops < 1 || minHops > graphMaxHops {
			return 0, 0, fmt.Errorf("invalid_hops")
		}
		if minHops > maxHops {
			return 0, 0, fmt.Errorf("invalid_hops")
		}
		if maxHops > graphMaxHops {
			maxHops = graphMaxHops
		}
		return minHops, maxHops, nil
	}
	if strings.Contains(value, "..") {
		parts := strings.SplitN(value, "..", 2)
		if len(parts) != 2 {
			return 0, 0, fmt.Errorf("invalid_hops")
		}
		minHops, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return 0, 0, fmt.Errorf("invalid_hops")
		}
		maxHops, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return 0, 0, fmt.Errorf("invalid_hops")
		}
		return normalize(minHops, maxHops)
	}
	if strings.Count(value, "-") == 1 && !strings.HasPrefix(value, "-") {
		parts := strings.SplitN(value, "-", 2)
		minHops, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return 0, 0, fmt.Errorf("invalid_hops")
		}
		maxHops, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return 0, 0, fmt.Errorf("invalid_hops")
		}
		return normalize(minHops, maxHops)
	}
	maxHops, err := strconv.Atoi(value)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid_hops")
	}
	return normalize(1, maxHops)
}

func graphNormalizeBranchLimit(limit int) int {
	switch {
	case limit <= 0:
		return graphDefaultBranch
	case limit > graphMaxBranch:
		return graphMaxBranch
	default:
		return limit
	}
}

func graphEdgeTraversalCost(weight float64) float64 {
	if weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
		return math.Inf(1)
	}
	return 1.0 / weight
}

func graphSplitCSV(raw string) ([]string, error) {
	var out []string
	start := 0
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\'' || raw[i] == '"' {
			end, _, err := graphQueryReadString(raw, i)
			if err != nil {
				return nil, err
			}
			i = end - 1
			continue
		}
		if raw[i] == ',' {
			out = append(out, strings.TrimSpace(raw[start:i]))
			start = i + 1
		}
	}
	if last := strings.TrimSpace(raw[start:]); last != "" {
		out = append(out, last)
	}
	return out, nil
}

func graphUnquote(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) >= 2 && (raw[0] == '\'' || raw[0] == '"') {
		if end, value, err := graphQueryReadString(raw, 0); err == nil && end == len(raw) {
			return value
		}
	}
	return raw
}

func graphLookupPropertyValue(props map[string]interface{}, path []string) (interface{}, bool) {
	if len(path) == 0 || props == nil {
		return nil, false
	}
	var current interface{} = props
	for _, segment := range path {
		obj, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		next, ok := obj[segment]
		if !ok {
			return nil, false
		}
		current = next
	}
	return current, true
}

func graphValueAsFloat(value interface{}) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0, false
		}
		return typed, true
	case float32:
		v := float64(typed)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		return v, true
	case int:
		return float64(typed), true
	case int8:
		return float64(typed), true
	case int16:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint8:
		return float64(typed), true
	case uint16:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return 0, false
		}
		return parsed, true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func graphValueAsBool(value interface{}) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes", "on":
			return true, true
		case "0", "false", "no", "off":
			return false, true
		default:
			return false, false
		}
	default:
		return false, false
	}
}

func graphPropertyValueAsString(value interface{}) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		if number, ok := graphValueAsFloat(value); ok {
			return strconv.FormatFloat(number, 'f', -1, 64)
		}
		return fmt.Sprintf("%v", value)
	}
}

func graphCompareString(actual string, op string, expected string) bool {
	switch op {
	case "=":
		return actual == expected
	case "!=":
		return actual != expected
	case ">":
		return actual > expected
	case ">=":
		return actual >= expected
	case "<":
		return actual < expected
	case "<=":
		return actual <= expected
	default:
		return false
	}
}

func graphCompareFloat(actual float64, op string, expected float64) bool {
	switch op {
	case "=":
		return actual == expected
	case "!=":
		return actual != expected
	case ">":
		return actual > expected
	case ">=":
		return actual >= expected
	case "<":
		return actual < expected
	case "<=":
		return actual <= expected
	default:
		return false
	}
}

func graphContainsLabel(labels []string, target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	for _, label := range labels {
		if label == target {
			return true
		}
	}
	return false
}

func (db *Database) graphPutNode(record GraphNodeRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = db.graphUpsertPairPayload(graphNodePairKey(record.ID), data, false)
	return err
}

func (db *Database) graphPutEdge(record GraphEdgeRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	edgeKey := graphEdgePairKey(record.From, record.To, record.Type, record.Directed)
	if _, err := db.graphUpsertPairPayload(edgeKey, data, false); err != nil {
		return err
	}
	if _, err := db.graphUpsertPairPayload(graphAdjOutPairKey(record.From, record.Type, record.To, record.Directed), edgeKey, true); err != nil {
		return err
	}
	if _, err := db.graphUpsertPairPayload(graphAdjInPairKey(record.To, record.Type, record.From, record.Directed), edgeKey, true); err != nil {
		return err
	}
	return nil
}

func (db *Database) graphGetNode(id string) (GraphNodeRecord, bool, error) {
	pairKey := graphNodePairKey(id)
	payload, found, err := db.graphGetPayloadByPairKey(pairKey)
	if err != nil {
		return GraphNodeRecord{}, false, err
	}
	if !found {
		return GraphNodeRecord{}, false, nil
	}
	var record GraphNodeRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		return GraphNodeRecord{}, false, err
	}
	return record, true, nil
}

func (db *Database) graphGetEdge(fromID string, toID string, edgeType string, directed bool) (GraphEdgeRecord, bool, error) {
	pairKey := graphEdgePairKey(fromID, toID, edgeType, directed)
	return db.graphGetEdgeByPairKey(pairKey)
}

func (db *Database) graphGetEdgeByPairKey(pairKey []byte) (GraphEdgeRecord, bool, error) {
	payload, found, err := db.graphGetPayloadByPairKey(pairKey)
	if err != nil {
		return GraphEdgeRecord{}, false, err
	}
	if !found {
		return GraphEdgeRecord{}, false, nil
	}
	var record GraphEdgeRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		return GraphEdgeRecord{}, false, err
	}
	return record, true, nil
}

func (db *Database) graphDeleteEdge(record GraphEdgeRecord) error {
	if err := db.graphDeleteEdgePropertyIndexes(record); err != nil {
		return err
	}
	edgeKey := graphEdgePairKey(record.From, record.To, record.Type, record.Directed)
	_, err := db.graphDeletePairAndPayload(edgeKey)
	if err != nil {
		return err
	}
	if _, err := db.graphDeletePairAndPayload(graphAdjOutPairKey(record.From, record.Type, record.To, record.Directed)); err != nil {
		return err
	}
	if _, err := db.graphDeletePairAndPayload(graphAdjInPairKey(record.To, record.Type, record.From, record.Directed)); err != nil {
		return err
	}
	return nil
}

func (db *Database) graphPutEdgePropertyIndexes(record GraphEdgeRecord) error {
	entries := graphBuildEdgeIndexEntries(record)
	if len(entries) == 0 {
		return nil
	}
	edgeKey := graphEdgePairKey(record.From, record.To, record.Type, record.Directed)
	for _, pairKey := range entries {
		if _, err := db.graphUpsertPairPayload(pairKey, edgeKey, true); err != nil {
			return err
		}
	}
	return nil
}

func (db *Database) graphDeleteEdgePropertyIndexes(record GraphEdgeRecord) error {
	entries := graphBuildEdgeIndexEntries(record)
	if len(entries) == 0 {
		return nil
	}
	for _, pairKey := range entries {
		if _, err := db.graphDeletePairAndPayload(pairKey); err != nil {
			return err
		}
	}
	return nil
}

func graphBuildEdgeIndexEntries(record GraphEdgeRecord) [][]byte {
	if record.Props == nil || len(record.Props) == 0 {
		return nil
	}
	edgeID := record.ID
	if edgeID == "" {
		edgeID = graphEdgeID(record.From, record.To, record.Type, record.Directed)
	}
	keys := make([][]byte, 0, len(record.Props))
	for propKey, rawValue := range record.Props {
		propKey = strings.TrimSpace(propKey)
		if propKey == "" {
			continue
		}
		token, ok := graphCanonicalPropertyToken(rawValue)
		if !ok {
			continue
		}
		keys = append(keys, graphEdgeIndexPairKey(propKey, token, edgeID))
	}
	return keys
}

func graphEdgeIndexPairKey(propKey string, valueToken string, edgeID string) []byte {
	return []byte(
		graphEdgeIndexPrefix +
			graphEncodeSegment(propKey) +
			"/" +
			graphEncodeSegment(valueToken) +
			"/" +
			graphEncodeSegment(edgeID),
	)
}

func graphEdgeIndexScanPrefix(propKey string, valueToken string) []byte {
	return []byte(
		graphEdgeIndexPrefix +
			graphEncodeSegment(propKey) +
			"/" +
			graphEncodeSegment(valueToken) +
			"/",
	)
}

func graphCanonicalPropertyToken(value interface{}) (string, bool) {
	switch typed := value.(type) {
	case string:
		return "s:" + typed, true
	case bool:
		if typed {
			return "b:1", true
		}
		return "b:0", true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return "", false
		}
		return "n:" + strconv.FormatFloat(typed, 'f', -1, 64), true
	case float32:
		v := float64(typed)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return "", false
		}
		return "n:" + strconv.FormatFloat(v, 'f', -1, 64), true
	case int:
		return "n:" + strconv.FormatInt(int64(typed), 10), true
	case int8:
		return "n:" + strconv.FormatInt(int64(typed), 10), true
	case int16:
		return "n:" + strconv.FormatInt(int64(typed), 10), true
	case int32:
		return "n:" + strconv.FormatInt(int64(typed), 10), true
	case int64:
		return "n:" + strconv.FormatInt(typed, 10), true
	case uint:
		return "n:" + strconv.FormatUint(uint64(typed), 10), true
	case uint8:
		return "n:" + strconv.FormatUint(uint64(typed), 10), true
	case uint16:
		return "n:" + strconv.FormatUint(uint64(typed), 10), true
	case uint32:
		return "n:" + strconv.FormatUint(uint64(typed), 10), true
	case uint64:
		return "n:" + strconv.FormatUint(typed, 10), true
	case json.Number:
		if parsed, err := typed.Float64(); err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) {
			return "n:" + strconv.FormatFloat(parsed, 'f', -1, 64), true
		}
		return "", false
	default:
		return "", false
	}
}

func (db *Database) graphDeleteNodeEdges(nodeID string) error {
	seen := map[string]struct{}{}
	deleteByPrefix := func(prefix []byte) error {
		var cursor []byte
		for {
			results, nextCursor, err := db.PairScanWithOptions(prefix, graphDefaultScanPage, cursor, true)
			if err != nil {
				return err
			}
			if len(results) == 0 {
				break
			}
			for _, res := range results {
				payload, err := db.readValuePayload(res.Key)
				if err != nil {
					continue
				}
				edge, found, err := db.graphGetEdgeByPairKey(payload)
				if err != nil || !found {
					continue
				}
				if _, ok := seen[edge.ID]; ok {
					continue
				}
				seen[edge.ID] = struct{}{}
				if err := db.graphDeleteEdge(edge); err != nil {
					return err
				}
			}
			if len(nextCursor) == 0 || len(results) < graphDefaultScanPage {
				break
			}
			cursor = nextCursor
		}
		return nil
	}
	if err := deleteByPrefix(graphAdjOutScanPrefix(nodeID, "")); err != nil {
		return err
	}
	return deleteByPrefix(graphAdjInScanPrefix(nodeID, ""))
}

func (db *Database) graphEnsureNode(id string) error {
	if id == "" {
		return fmt.Errorf("empty_node_id")
	}
	if _, ok, err := db.graphGetNode(id); err != nil {
		return err
	} else if ok {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := GraphNodeRecord{
		ID:        id,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.graphPutNode(record); err != nil {
		return err
	}
	return db.graphSyncNodeTerms(nil, &record)
}

// Le tre funzioni qui sotto sono rimaste come nomi della famiglia graph, ma il
// corpo è la composizione generica "nome + payload" di commands.go: la
// condividono con le righe delle record table.
func (db *Database) graphGetPayloadByPairKey(pairKey []byte) ([]byte, bool, error) {
	return db.getPairPayload(pairKey)
}

// Ogni scrittura del grafo passa da questi due wrapper — record, indici di
// adiacenza, indici di proprietà, indice lessicale — quindi è qui che si fa
// avanzare l'epoca che invecchia le convergenze in cache (graph_cache.go). Un
// percorso di scrittura nuovo la fa avanzare senza doversene ricordare, che è
// esattamente ciò che il resto degli indici derivati *non* garantisce.
//
// La cache scrive invece con `upsertPairPayload` diretto: le sue righe non sono
// grafo, e farle contare come tali la invaliderebbe a ogni riga che scrive.
func (db *Database) graphUpsertPairPayload(pairKey []byte, payload []byte, hidden bool) (uint64, error) {
	key, err := db.upsertPairPayload(pairKey, payload, hidden)
	if err == nil {
		db.graphCacheBumpEpoch()
	}
	return key, err
}

func (db *Database) graphDeletePairAndPayload(pairKey []byte) (bool, error) {
	deleted, err := db.deletePairAndPayload(pairKey)
	if err == nil && deleted {
		db.graphCacheBumpEpoch()
	}
	return deleted, err
}

func (db *Database) graphScanAdjacency(
	prefix []byte,
	limit int,
	cursor []byte,
	filter func(*GraphEdgeRecord) bool,
) ([]GraphEdgeRecord, []byte, error) {
	limit = graphNormalizeLimit(limit)
	out := make([]GraphEdgeRecord, 0, limit)
	pageSize := graphDefaultScanPage
	if limit < pageSize {
		pageSize = limit
	}
	if pageSize < 1 {
		pageSize = 1
	}
	var currentCursor []byte
	if len(cursor) > 0 {
		currentCursor = append([]byte{}, cursor...)
	}
	pageCount := 0
	for len(out) < limit {
		results, nextCursor, err := db.PairScanWithOptions(prefix, pageSize, currentCursor, true)
		if err != nil {
			return nil, nil, err
		}
		if len(results) == 0 {
			return out, nil, nil
		}
		for index, res := range results {
			edgePairKey, err := db.readValuePayload(res.Key)
			if err != nil || len(edgePairKey) == 0 {
				continue
			}
			edge, found, err := db.graphGetEdgeByPairKey(edgePairKey)
			if err != nil || !found {
				continue
			}
			if filter != nil && !filter(&edge) {
				continue
			}
			out = append(out, edge)
			if len(out) >= limit {
				// Il filtro può riempire l'output a metà della pagina interna:
				// ripartire da nextCursor salterebbe i risultati non consumati.
				if index+1 < len(results) {
					return out, append([]byte(nil), res.Value...), nil
				}
				return out, nextCursor, nil
			}
		}
		pageCount++
		if len(nextCursor) == 0 || len(results) < pageSize {
			return out, nil, nil
		}
		if pageCount >= graphMaxScanPages {
			return out, nextCursor, nil
		}
		currentCursor = nextCursor
	}
	return out, nil, nil
}

func (db *Database) graphCountAdjacencyPrefix(prefix []byte, weighted bool) (int, float64, error) {
	limit := graphDefaultScanPage
	var cursor []byte
	totalCount := 0
	totalWeight := 0.0
	pageCount := 0
	for {
		results, nextCursor, err := db.PairScanWithOptions(prefix, limit, cursor, true)
		if err != nil {
			return totalCount, totalWeight, err
		}
		if len(results) == 0 {
			return totalCount, totalWeight, nil
		}
		totalCount += len(results)
		if weighted {
			for _, res := range results {
				edgePairKey, err := db.readValuePayload(res.Key)
				if err != nil || len(edgePairKey) == 0 {
					continue
				}
				edge, found, err := db.graphGetEdgeByPairKey(edgePairKey)
				if err != nil || !found {
					continue
				}
				totalWeight += edge.Weight
			}
		}
		pageCount++
		if len(nextCursor) == 0 || len(results) < limit {
			return totalCount, totalWeight, nil
		}
		if pageCount >= graphMaxScanPages*32 {
			return totalCount, totalWeight, nil
		}
		cursor = nextCursor
	}
}

func (db *Database) graphScanNeighborTypes(
	prefix []byte,
	limit int,
	cursor []byte,
	direction string,
	weighted bool,
) ([]graphRelationTypeCount, []byte, error) {
	limit = graphNormalizeLimit(limit)
	results, nextCursor, err := db.PairScanWithOptions(prefix, limit, cursor, true)
	if err != nil {
		return nil, nil, err
	}
	buckets := make(map[string]*graphRelationTypeCount, len(results))
	for _, res := range results {
		relType, ok := graphRelationFromAdjacencyKey(res.Value, direction)
		if !ok || relType == "" {
			continue
		}
		bucket, exists := buckets[relType]
		if !exists {
			bucket = &graphRelationTypeCount{Type: relType}
			buckets[relType] = bucket
		}
		bucket.Count++
		if weighted {
			edgePairKey, err := db.readValuePayload(res.Key)
			if err != nil || len(edgePairKey) == 0 {
				continue
			}
			edge, found, err := db.graphGetEdgeByPairKey(edgePairKey)
			if err != nil || !found {
				continue
			}
			bucket.Weighted += edge.Weight
		}
	}
	counts := make([]graphRelationTypeCount, 0, len(buckets))
	for _, bucket := range buckets {
		counts = append(counts, *bucket)
	}
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].Count == counts[j].Count {
			if counts[i].Weighted == counts[j].Weighted {
				return counts[i].Type < counts[j].Type
			}
			return counts[i].Weighted > counts[j].Weighted
		}
		return counts[i].Count > counts[j].Count
	})
	return counts, nextCursor, nil
}

func graphRelationFromAdjacencyKey(raw []byte, direction string) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	text := string(raw)
	var trimmed string
	switch direction {
	case "out":
		if !strings.HasPrefix(text, graphAdjOutPrefix) {
			return "", false
		}
		trimmed = strings.TrimPrefix(text, graphAdjOutPrefix)
	case "in":
		if !strings.HasPrefix(text, graphAdjInPrefix) {
			return "", false
		}
		trimmed = strings.TrimPrefix(text, graphAdjInPrefix)
	default:
		return "", false
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) < 4 {
		return "", false
	}
	relEncoded := parts[1]
	decoded, err := base64.RawURLEncoding.DecodeString(relEncoded)
	if err != nil {
		return "", false
	}
	return string(decoded), true
}

func mergeGraphNeighborTypeCounts(left []graphRelationTypeCount, right []graphRelationTypeCount) []graphRelationTypeCount {
	merged := make(map[string]graphRelationTypeCount, len(left)+len(right))
	for _, item := range left {
		merged[item.Type] = item
	}
	for _, item := range right {
		existing := merged[item.Type]
		existing.Type = item.Type
		existing.Count += item.Count
		existing.Weighted += item.Weighted
		merged[item.Type] = existing
	}
	out := make([]graphRelationTypeCount, 0, len(merged))
	for _, item := range merged {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count == out[j].Count {
			if out[i].Weighted == out[j].Weighted {
				return out[i].Type < out[j].Type
			}
			return out[i].Weighted > out[j].Weighted
		}
		return out[i].Count > out[j].Count
	})
	return out
}

func graphFormatEdgesResponse(edges []GraphEdgeRecord, nextCursor []byte) (string, error) {
	payload, err := graphEncodeJSON(edges)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("SUCCESS,count=%d,next_cursor=%s,payload=%s", len(edges), graphCursorToken(nextCursor), payload), nil
}

func graphFormatNeighborTypesResponse(counts []graphRelationTypeCount, nextCursor []byte) (string, error) {
	payload, err := graphEncodeJSON(counts)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("SUCCESS,count=%d,next_cursor=%s,payload=%s", len(counts), graphCursorToken(nextCursor), payload), nil
}

func graphEncodeJSON(value interface{}) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(encoded), nil
}

func graphParseProps(raw string) (map[string]interface{}, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var data []byte
	if strings.HasPrefix(raw, "{") {
		data = []byte(raw)
	} else {
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			decoded, err = base64.RawStdEncoding.DecodeString(raw)
			if err != nil {
				return nil, err
			}
		}
		data = decoded
	}
	var props map[string]interface{}
	if err := json.Unmarshal(data, &props); err != nil {
		return nil, err
	}
	return props, nil
}

func graphParseReferences(raw string) ([]GraphReferenceSentence, bool, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, false, nil
	}
	if trimmed == graphClearToken {
		return nil, true, nil
	}
	data, err := graphDecodeMaybeBase64JSON(trimmed)
	if err != nil {
		return nil, true, err
	}
	var decoded []GraphReferenceSentence
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, true, err
	}
	if len(decoded) > graphMaxReferences {
		return nil, true, fmt.Errorf("too_many_references")
	}
	out := make([]GraphReferenceSentence, 0, len(decoded))
	seen := make(map[string]struct{}, len(decoded))
	total := 0
	for index, reference := range decoded {
		reference.Text = strings.TrimSpace(reference.Text)
		reference.Source = strings.TrimSpace(reference.Source)
		if reference.Text == "" {
			return nil, true, fmt.Errorf("reference_%d_requires_text", index)
		}
		if len(reference.Text) > graphMaxReferenceLen {
			return nil, true, fmt.Errorf("reference_%d_too_long", index)
		}
		total += len(reference.Text)
		if total > graphMaxReferenceAll {
			return nil, true, fmt.Errorf("references_too_large")
		}
		if reference.Ordinal < 0 {
			return nil, true, fmt.Errorf("reference_%d_invalid_ordinal", index)
		}
		if strings.TrimSpace(reference.ID) == "" {
			sum := sha256.Sum256([]byte(reference.Text))
			reference.ID = "ref_" + hex.EncodeToString(sum[:10])
		} else {
			reference.ID = strings.TrimSpace(reference.ID)
		}
		if _, ok := seen[reference.ID]; ok {
			continue
		}
		seen[reference.ID] = struct{}{}
		out = append(out, reference)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Ordinal != out[j].Ordinal {
			return out[i].Ordinal < out[j].Ordinal
		}
		return out[i].ID < out[j].ID
	})
	return out, true, nil
}

func graphParseLabels(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		label := strings.TrimSpace(part)
		if label == "" {
			continue
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

func graphNormalizeID(raw string) string {
	return strings.TrimSpace(raw)
}

func graphNormalizeEdgeType(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if value == "*" {
		return "*"
	}
	return value
}

func graphEncodeSegment(raw string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// graphDecodeSegment è l'inverso: serve a chi legge una chiave del namespace e
// deve tornare all'id che ci era stato codificato dentro (graph_cache.go).
func graphDecodeSegment(encoded string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

func graphNodePairKey(nodeID string) []byte {
	return []byte(graphNodePrefix + graphEncodeSegment(nodeID))
}

func graphEdgePairKey(fromID string, toID string, edgeType string, directed bool) []byte {
	typePart := graphEncodeSegment(edgeType)
	return []byte(fmt.Sprintf("%s%d/%s/%s/%s", graphEdgePrefix, boolToInt(directed), graphEncodeSegment(fromID), typePart, graphEncodeSegment(toID)))
}

func graphAdjOutPairKey(fromID string, edgeType string, toID string, directed bool) []byte {
	typePart := graphEncodeSegment(edgeType)
	return []byte(fmt.Sprintf("%s%s/%s/%s/%d", graphAdjOutPrefix, graphEncodeSegment(fromID), typePart, graphEncodeSegment(toID), boolToInt(directed)))
}

func graphAdjInPairKey(toID string, edgeType string, fromID string, directed bool) []byte {
	typePart := graphEncodeSegment(edgeType)
	return []byte(fmt.Sprintf("%s%s/%s/%s/%d", graphAdjInPrefix, graphEncodeSegment(toID), typePart, graphEncodeSegment(fromID), boolToInt(directed)))
}

func graphAdjOutScanPrefix(fromID string, edgeType string) []byte {
	base := graphAdjOutPrefix + graphEncodeSegment(fromID) + "/"
	if edgeType == "" {
		return []byte(base)
	}
	return []byte(base + graphEncodeSegment(edgeType) + "/")
}

func graphAdjInScanPrefix(toID string, edgeType string) []byte {
	base := graphAdjInPrefix + graphEncodeSegment(toID) + "/"
	if edgeType == "" {
		return []byte(base)
	}
	return []byte(base + graphEncodeSegment(edgeType) + "/")
}

func graphEdgeID(fromID string, toID string, edgeType string, directed bool) string {
	raw := fmt.Sprintf("%d|%s|%s|%s", boolToInt(directed), fromID, edgeType, toID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func graphNormalizeLimit(limit int) int {
	switch {
	case limit <= 0:
		return graphDefaultLimit
	case limit > graphMaxLimit:
		return graphMaxLimit
	default:
		return limit
	}
}

func graphParseCursorToken(raw string) ([]byte, error) {
	token := strings.TrimSpace(raw)
	if token == "" || token == "*" {
		return nil, nil
	}
	if strings.HasPrefix(token, "x") && len(token) > 1 {
		decoded, err := hex.DecodeString(token[1:])
		if err != nil {
			return nil, err
		}
		return decoded, nil
	}
	if strings.HasPrefix(token, "base64:") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(token, "base64:"))
		if err != nil {
			return nil, err
		}
		return decoded, nil
	}
	return []byte(token), nil
}

func graphCursorToken(cursor []byte) string {
	if len(cursor) == 0 {
		return "*"
	}
	if bytes.IndexByte(cursor, ',') >= 0 || bytes.IndexByte(cursor, ' ') >= 0 {
		return "base64:" + base64.StdEncoding.EncodeToString(cursor)
	}
	return "x" + hex.EncodeToString(cursor)
}
