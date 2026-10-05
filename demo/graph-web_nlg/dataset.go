package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"sort"
	"strings"
)

// Keep RDF spelling (including quotes, case and underscores): normalization can
// silently merge a literal with an entity or two distinct dataset values.
type triple struct {
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
	Object    string `json:"object"`
}
type example struct {
	Input    []string `json:"input"`
	Target   []string `json:"target"`
	Category string   `json:"category"`
	ID       string   `json:"webnlg-id"`
	Triples  []triple `json:"-"`
}
type reference struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Source string `json:"source"`
}
type node struct {
	ID         string            `json:"id"`
	Props      map[string]string `json:"props"`
	References []reference       `json:"references"`
}
type edge struct {
	From     string  `json:"from"`
	Type     string  `json:"type"`
	To       string  `json:"to"`
	Directed bool    `json:"directed"`
	Weight   float64 `json:"weight"`
}
type corpus struct {
	Nodes             []node
	Edges             []edge
	Facts             map[triple]bool
	ReferencesDropped int
}

func parseTriple(raw string) (triple, error) {
	parts := strings.Split(raw, " | ")
	if len(parts) != 3 {
		return triple{}, fmt.Errorf("invalid triple %q: expected subject | predicate | object", raw)
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
		if parts[i] == "" {
			return triple{}, fmt.Errorf("empty triple component in %q", raw)
		}
	}
	return triple{parts[0], parts[1], parts[2]}, nil
}
func token(kind, raw string) string {
	return "wn:" + kind + ":" + base64.RawURLEncoding.EncodeToString([]byte(raw))
}
func tripleEdge(t triple) edge {
	return edge{token("e", t.Subject), token("r", t.Predicate), token("e", t.Object), true, 1}
}
func edgeTriple(e edge) (triple, error) {
	decode := func(kind, s string) (string, error) {
		p := "wn:" + kind + ":"
		if !strings.HasPrefix(s, p) {
			return "", fmt.Errorf("unexpected graph token %q", s)
		}
		b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, p))
		return string(b), err
	}
	s, err := decode("e", e.From)
	if err != nil {
		return triple{}, err
	}
	p, err := decode("r", e.Type)
	if err != nil {
		return triple{}, err
	}
	o, err := decode("e", e.To)
	return triple{s, p, o}, err
}

// Reservoir sampling spans all categories instead of taking the airport-only
// prefix of the sorted export. Decode one entry at a time; 0 explicitly means all.
func loadExamples(path string, limit int, seed int64) ([]example, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return nil, 0, fmt.Errorf("%s: expected dataset object", path)
	}
	rng := rand.New(rand.NewSource(seed))
	rows := 0
	found := false
	language := ""
	var out []example
	ids := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, rows, err
		}
		if key == "language" {
			if err := d.Decode(&language); err != nil {
				return nil, rows, err
			}
			continue
		}
		if key != "values" {
			var skip json.RawMessage
			if err := d.Decode(&skip); err != nil {
				return nil, rows, err
			}
			continue
		}
		if found {
			return nil, rows, fmt.Errorf("duplicate values field")
		}
		found = true
		a, err := d.Token()
		if err != nil || a != json.Delim('[') {
			return nil, rows, fmt.Errorf("values must be an array")
		}
		for d.More() {
			var e example
			if err := d.Decode(&e); err != nil {
				return nil, rows, err
			}
			rows++
			if e.ID == "" || ids[e.ID] || len(e.Input) == 0 || len(e.Target) == 0 {
				return nil, rows, fmt.Errorf("row %d: missing/duplicate id, triples or references", rows)
			}
			ids[e.ID] = true
			for _, raw := range e.Input {
				t, err := parseTriple(raw)
				if err != nil {
					return nil, rows, fmt.Errorf("%s: %w", e.ID, err)
				}
				e.Triples = append(e.Triples, t)
			}
			for _, s := range e.Target {
				if strings.TrimSpace(s) == "" || len(s) > 4096 {
					return nil, rows, fmt.Errorf("%s: empty or oversized reference", e.ID)
				}
			}
			if limit == 0 || len(out) < limit {
				out = append(out, e)
			} else if j := rng.Intn(rows); j < limit {
				out[j] = e
			}
		}
		if _, err := d.Token(); err != nil {
			return nil, rows, err
		}
	}
	if _, err := d.Token(); err != nil {
		return nil, rows, err
	}
	var tail any
	if err := d.Decode(&tail); err != io.EOF {
		return nil, rows, fmt.Errorf("unexpected content after dataset")
	}
	if !found || rows == 0 || language != "en" {
		return nil, rows, fmt.Errorf("expected nonempty English WebNLG export")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, rows, nil
}

func buildCorpus(examples []example) corpus {
	c := corpus{Facts: map[triple]bool{}}
	nodes := map[string]*node{}
	refSeen := map[string]map[string]bool{}
	for _, e := range examples {
		entities := map[string]bool{}
		for _, t := range e.Triples {
			if !c.Facts[t] {
				c.Facts[t] = true
				c.Edges = append(c.Edges, tripleEdge(t))
			}
			entities[t.Subject] = true
			entities[t.Object] = true
		}
		for raw := range entities {
			id := token("e", raw)
			n := nodes[id]
			if n == nil {
				n = &node{ID: id, Props: map[string]string{"rdf_value": raw}, References: []reference{{ID: "name", Text: strings.ReplaceAll(raw, "_", " "), Source: "webnlg:entity"}}}
				nodes[id] = n
				refSeen[id] = map[string]bool{}
			}
			for i, s := range e.Target {
				rid := fmt.Sprintf("%s/%d", e.ID, i)
				if refSeen[id][rid] {
					continue
				}
				refSeen[id][rid] = true
				// Leave a small, deterministic evidence window on hubs; never truncate sentences.
				if len(n.References) >= 9 {
					c.ReferencesDropped++
					continue
				}
				n.References = append(n.References, reference{rid, s, e.ID})
			}
		}
	}
	for _, n := range nodes {
		c.Nodes = append(c.Nodes, *n)
	}
	sort.Slice(c.Nodes, func(i, j int) bool { return c.Nodes[i].ID < c.Nodes[j].ID })
	sort.Slice(c.Edges, func(i, j int) bool {
		a, b := c.Edges[i], c.Edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return a.To < b.To
	})
	return c
}
