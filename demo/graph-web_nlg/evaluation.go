package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type extractionMetrics struct {
	Examples   int     `json:"examples"`
	Submitted  int     `json:"submitted"`
	Gold       int     `json:"gold_triples"`
	Predicted  int     `json:"predicted_triples"`
	Correct    int     `json:"correct_triples"`
	Precision  float64 `json:"precision"`
	Recall     float64 `json:"recall"`
	F1         float64 `json:"f1"`
	ExactMatch float64 `json:"exact_match"`
}

// Missing predictions count as empty; duplicates and unknown ids are errors.
// This measures exact RDF extraction, not prose quality or an LLM's reasoning.
func scorePredictions(path string, cases []evaluationCase) (extractionMetrics, error) {
	m := extractionMetrics{Examples: len(cases)}
	gold := map[string]map[triple]bool{}
	for _, c := range cases {
		g := map[triple]bool{}
		for _, t := range c.Gold {
			g[t] = true
		}
		gold[c.ID] = g
		m.Gold += len(g)
	}
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	seen := map[string]bool{}
	exact := 0
	for {
		var p struct {
			ID      string   `json:"id"`
			Triples []triple `json:"triples"`
		}
		err := d.Decode(&p)
		if err == io.EOF {
			break
		}
		if err != nil {
			return m, err
		}
		g, ok := gold[p.ID]
		if !ok || seen[p.ID] {
			return m, fmt.Errorf("unknown or duplicate prediction id %q", p.ID)
		}
		seen[p.ID] = true
		m.Submitted++
		unique := map[triple]bool{}
		correct := 0
		for _, t := range p.Triples {
			if t.Subject == "" || t.Predicate == "" || t.Object == "" {
				return m, fmt.Errorf("incomplete predicted triple for %s", p.ID)
			}
			unique[t] = true
		}
		for t := range unique {
			if g[t] {
				correct++
			}
		}
		m.Correct += correct
		m.Predicted += len(unique)
		if correct == len(g) && correct == len(unique) {
			exact++
		}
	}
	m.Precision = ratio(m.Correct, m.Predicted)
	m.Recall = ratio(m.Correct, m.Gold)
	if m.Precision+m.Recall > 0 {
		m.F1 = 2 * m.Precision * m.Recall / (m.Precision + m.Recall)
	}
	m.ExactMatch = ratio(exact, m.Examples)
	return m, nil
}
