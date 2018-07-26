package scheduler

import (
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// ImplicitTarget is used to represent any remaining attribute values
	// when target percentages don't add up to 100
	ImplicitTarget = "*"
)

// SpreadIterator is used to spread allocations across a specified attribute
// according to preset weights
type SpreadIterator struct {
	ctx        Context
	source     RankIterator
	job        *structs.Job
	tg         *structs.TaskGroup
	jobSpreads []*structs.Spread
	// tgSpreadInfo is a map per task group with precomputed
	// values for desired counts and weight
	tgSpreadInfo map[string]spreadAttributeMap
	// sumSpreadWeights tracks the total weight across all spread
	// stanzas
	sumSpreadWeights int
	hasSpread        bool
	// groupProperySets is a memoized map from task group to property sets.
	// existing allocs are computed once, and allocs from the plan are updated
	// when Reset is called
	groupPropertySets map[string][]*propertySet
}

type spreadAttributeMap map[string]*spreadInfo

type spreadInfo struct {
	weight        int
	desiredCounts map[string]float64
}

func NewSpreadIterator(ctx Context, source RankIterator) *SpreadIterator {
	iter := &SpreadIterator{
		ctx:               ctx,
		source:            source,
		groupPropertySets: make(map[string][]*propertySet),
		tgSpreadInfo:      make(map[string]spreadAttributeMap),
	}
	return iter
}

func (iter *SpreadIterator) Reset() {
	iter.source.Reset()
	for _, sets := range iter.groupPropertySets {
		for _, ps := range sets {
			ps.PopulateProposed()
		}
	}
}

func (iter *SpreadIterator) SetJob(job *structs.Job) {
	iter.job = job
	if job.Spreads != nil {
		iter.jobSpreads = job.Spreads
	}
}

func (iter *SpreadIterator) SetTaskGroup(tg *structs.TaskGroup) {
	iter.tg = tg

	// Build the property set at the taskgroup level
	if _, ok := iter.groupPropertySets[tg.Name]; !ok {
		// First add property sets that are at the job level for this task group
		for _, spread := range iter.jobSpreads {
			pset := NewPropertySet(iter.ctx, iter.job)
			pset.SetTargetAttribute(spread.Attribute, tg.Name)
			iter.groupPropertySets[tg.Name] = append(iter.groupPropertySets[tg.Name], pset)
		}

		// Include property sets at the task group level
		for _, spread := range tg.Spreads {
			pset := NewPropertySet(iter.ctx, iter.job)
			pset.SetTargetAttribute(spread.Attribute, tg.Name)
			iter.groupPropertySets[tg.Name] = append(iter.groupPropertySets[tg.Name], pset)
		}
	}

	// Check if there are any spreads configured
	iter.hasSpread = len(iter.groupPropertySets[tg.Name]) != 0

	// Build tgSpreadInfo at the task group level
	if _, ok := iter.tgSpreadInfo[tg.Name]; !ok {
		iter.computeSpreadInfo(tg)
	}

}

func (iter *SpreadIterator) hasSpreads() bool {
	return iter.hasSpread
}

func (iter *SpreadIterator) Next() *RankedNode {
	for {
		option := iter.source.Next()

		// Hot path if there is nothing to check
		if option == nil || !iter.hasSpreads() {
			return option
		}

		tgName := iter.tg.Name
		propertySets := iter.groupPropertySets[tgName]
		// Iterate over each spread attribute's property set and add a weighted score
		totalSpreadScore := 0.0
		for _, pset := range propertySets {
			nValue, errorMsg, usedCount := pset.UsedCount(option.Node, tgName)
			// Skip if there was errors in resolving this attribute to compute used counts
			if errorMsg != "" {
				iter.ctx.Logger().Printf("[WARN] sched: error building spread attributes for task group %v:%v", tgName, errorMsg)
			}
			spreadAttributeMap := iter.tgSpreadInfo[tgName]
			spreadDetails := spreadAttributeMap[pset.targetAttribute]
			// Get the desired count
			desiredCount, ok := spreadDetails.desiredCounts[nValue]
			if !ok {
				// See if there is an implicit target
				desiredCount, ok = spreadDetails.desiredCounts[ImplicitTarget]
				if !ok {
					// The desired count for this attribute is zero if it gets here
					// don't boost the score
					continue
				}
			}
			if float64(usedCount) < desiredCount {
				// Calculate the relative weight of this specific spread attribute
				spreadWeight := float64(spreadDetails.weight) / float64(iter.sumSpreadWeights)
				// Score Boost is proportional the difference between current and desired count
				// It is multiplied with the spread weight to account for cases where the job has
				// more than one spread attribute
				scoreBoost := ((desiredCount - float64(usedCount)) / desiredCount) * spreadWeight
				totalSpreadScore += scoreBoost
			}
		}

		if totalSpreadScore != 0.0 {
			option.Scores = append(option.Scores, totalSpreadScore)
			iter.ctx.Metrics().ScoreNode(option.Node, "allocation-spread", totalSpreadScore)
		}
		return option
	}
}

// computeSpreadInfo computes and stores percentages and total values
// from all spreads that apply to a specific task group
func (iter *SpreadIterator) computeSpreadInfo(tg *structs.TaskGroup) {
	spreadInfos := make(spreadAttributeMap, len(tg.Spreads))
	totalCount := tg.Count

	// Always combine any spread stanzas defined at the job level here
	combinedSpreads := make([]*structs.Spread, 0, len(tg.Spreads)+len(iter.jobSpreads))
	combinedSpreads = append(combinedSpreads, tg.Spreads...)
	combinedSpreads = append(combinedSpreads, iter.jobSpreads...)
	for _, spread := range combinedSpreads {
		si := &spreadInfo{weight: spread.Weight, desiredCounts: make(map[string]float64)}
		sumDesiredCounts := 0.0
		for _, st := range spread.SpreadTarget {
			desiredCount := (float64(st.Percent) / float64(100)) * float64(totalCount)
			si.desiredCounts[st.Value] = desiredCount
			sumDesiredCounts += desiredCount
		}
		if sumDesiredCounts < float64(totalCount) {
			remainingCount := float64(totalCount) - sumDesiredCounts
			si.desiredCounts[ImplicitTarget] = remainingCount
		}
		spreadInfos[spread.Attribute] = si
		iter.sumSpreadWeights += spread.Weight
	}
	iter.tgSpreadInfo[tg.Name] = spreadInfos
}
