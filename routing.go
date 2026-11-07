package main

import (
	"math/rand"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

// Routing state learned from real wrapper-lite responses. The manager no
// longer asks Apple's catalog whether an item is available; it tries
// candidates and remembers which instance actually served each item.
//
//   - affinityCache: the instance that most recently served an adamId. A short
//     TTL keeps repeat requests on a known-good instance.
//   - negativeCache: (adamId, instance) pairs that recently failed at the
//     business layer. Those instances are tried last for that item, so a
//     request does not spend all of its attempts on known-bad candidates.
//
// Both caches are bounded and expire; when they are empty the manager simply
// tries a shuffled set of selectable instances.
var (
	affinityCache = expirable.NewLRU[string, string](100_000, nil, 30*time.Minute)
	negativeCache = expirable.NewLRU[string, struct{}](200_000, nil, 10*time.Minute)
)

func negativeKey(adamId, instanceId string) string {
	return adamId + "|" + instanceId
}

// noteInstanceSuccess records that instanceId served adamId successfully.
func noteInstanceSuccess(adamId, instanceId string) {
	if adamId == "" || instanceId == "" {
		return
	}
	affinityCache.Add(adamId, instanceId)
	negativeCache.Remove(negativeKey(adamId, instanceId))
}

// noteInstanceFailure records that instanceId failed at the business layer for
// adamId, and clears a now-stale affinity entry.
func noteInstanceFailure(adamId, instanceId string) {
	if adamId == "" || instanceId == "" {
		return
	}
	negativeCache.Add(negativeKey(adamId, instanceId), struct{}{})
	if id, ok := affinityCache.Get(adamId); ok && id == instanceId {
		affinityCache.Remove(adamId)
	}
}

// selectableInstances returns the instances that may serve a request: a known
// storefront region, a live process, and a closed FairPlay circuit. Instances
// still starting or logged out (empty Region) are skipped.
func selectableInstances() []*WrapperInstance {
	all := SnapshotInstances()
	out := make([]*WrapperInstance, 0, len(all))
	for _, inst := range all {
		if inst == nil || inst.Region == "" {
			continue
		}
		if circuitOpen(inst.Id) {
			continue
		}
		out = append(out, inst)
	}
	return out
}

// orderCandidates returns the candidate ids for one item: the affinity
// instance first, then instances not marked negative, then negative ones.
// Each group is shuffled so concurrent requests spread across instances.
func orderCandidates(instances []*WrapperInstance, adamId string) []string {
	if len(instances) == 0 {
		return nil
	}
	ids := make([]string, 0, len(instances))
	seen := make(map[string]bool, len(instances))

	if preferred, ok := affinityCache.Get(adamId); ok {
		for _, inst := range instances {
			if inst.Id == preferred {
				ids = append(ids, inst.Id)
				seen[inst.Id] = true
				break
			}
		}
	}

	var fresh, negative []string
	for _, inst := range instances {
		if seen[inst.Id] {
			continue
		}
		if _, bad := negativeCache.Get(negativeKey(adamId, inst.Id)); bad {
			negative = append(negative, inst.Id)
		} else {
			fresh = append(fresh, inst.Id)
		}
	}
	rand.Shuffle(len(fresh), func(i, j int) { fresh[i], fresh[j] = fresh[j], fresh[i] })
	rand.Shuffle(len(negative), func(i, j int) { negative[i], negative[j] = negative[j], negative[i] })
	ids = append(ids, fresh...)
	ids = append(ids, negative...)
	return ids
}

// candidateIDs returns all selectable instances in routing order for adamId.
func candidateIDs(adamId string) []string {
	return orderCandidates(selectableInstances(), adamId)
}

// lyricsRouteKey identifies one lyrics routing decision. Language and script
// are part of the key so a 404 for "ja" does not steer a later "en" request
// away from an instance that can serve it.
func lyricsRouteKey(adamId, language, script string) string {
	if script == "" {
		script = "en-Latn"
	}
	return "lyrics|" + adamId + "|" + language + "|" + script
}

// lyricsCandidateIDs returns candidates for /lyrics: instances whose
// storefront advertises the requested language first, then the rest. Both
// groups keep the affinity/negative ordering used by other endpoints.
func lyricsCandidateIDs(adamId, language, script string) []string {
	instances := selectableInstances()
	if len(instances) == 0 {
		return nil
	}
	routeKey := lyricsRouteKey(adamId, language, script)
	langs := ensureStorefrontLangs()
	var tier1, tier2 []*WrapperInstance
	for _, inst := range instances {
		if langs != nil && langs.regionSupportsLang(inst.Region, language) {
			tier1 = append(tier1, inst)
		} else {
			tier2 = append(tier2, inst)
		}
	}
	ids := orderCandidates(tier1, routeKey)
	ids = append(ids, orderCandidates(tier2, routeKey)...)
	return ids
}
