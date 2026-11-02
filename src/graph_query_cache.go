package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Solo 64 impronte recenti, senza mappe crescenti né nuove tabelle. La prima
// osservazione non scrive nulla; si ammette soltanto un controllo ricorrente
// che ha consumato almeno otto unità di lavoro. Collisioni = cache più fredda.
type graphQueryHistory struct {
	Signature [32]byte
	Used      uint32
	Visits    uint8
	Cost      int
}

func (exec *graphQueryExecution) associationExists(id string, rel graphQueryRelation) (bool, error) {
	store := exec.db.graphCacheOrNil()
	var digest [32]byte
	var signature string
	var epoch uint64
	class, visits := 0, uint8(0)
	pointProbe := rel.EdgeType != "" && rel.Target.ID != ""
	if !pointProbe && !exec.plan.CacheOff && store != nil && exec.cacheOps < store.config().Budget && !store.busy() {
		exec.cacheOps++
		raw, _ := json.Marshal([]string{"sql-exists-v1", id, rel.Direction, rel.EdgeType, rel.Target.ID, rel.Target.Label})
		digest = sha256.Sum256(raw)
		signature = "sql-exists-v1-" + hex.EncodeToString(digest[:])
		class = int(digest[0]) % graphCacheClassSlots
		epoch = store.currentEpoch()
		now := store.unixNow()
		store.mu.Lock()
		history := &store.queryHistory[class]
		if history.Signature != digest || now-history.Used > 300 {
			*history = graphQueryHistory{Signature: digest}
		}
		if history.Visits < 255 {
			history.Visits++
		}
		history.Used = now
		visits = history.Visits
		cost := history.Cost
		store.mu.Unlock()
		if visits > 1 && cost >= 8 {
			if members, found := store.lookupCommon(signature, class); found && len(members) == 1 {
				if members[0].ID == "yes" || members[0].ID == "no" {
					return members[0].ID == "yes", nil
				}
			}
		}
	}
	start, found := exec.work, false
	_, err := exec.scan(graphQueryAdjPrefix(id, rel.EdgeType, rel.Direction, rel.Target), nil, func(edge GraphEdgeRecord, _ []byte) (bool, error) {
		endpoint := edge.To
		if rel.Direction == "in" {
			endpoint = edge.From
		}
		var err error
		found, err = exec.matchNode(endpoint, rel.Target)
		return !found, err
	})
	if err != nil {
		return false, err
	}
	if signature != "" {
		cost := exec.work - start
		store.mu.Lock()
		history := &store.queryHistory[class]
		if history.Signature == digest {
			history.Cost = cost
		}
		store.mu.Unlock()
		if visits >= 2 && cost >= 8 && epoch == store.currentEpoch() && store.roll() < store.admissionProbability(1, 2, class) {
			answer := "no"
			if found {
				answer = "yes"
			}
			store.ensureMaintainer()
			// L'epoca è quella prima della scansione. Una scrittura concorrente
			// può rendere la voce stantia, mai farla sembrare più fresca dei dati.
			store.observeCommonAtEpoch(signature, []graphCacheMember{{ID: answer, Score: 1}}, class, epoch)
		}
	}
	return found, nil
}
