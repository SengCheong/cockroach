// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License included
// in the file licenses/BSL.txt and at www.mariadb.com/bsl11.
//
// Change Date: 2022-10-01
//
// On the date above, in accordance with the Business Source License, use
// of this software will be governed by the Apache License, Version 2.0,
// included in the file licenses/APL.txt and at
// https://www.apache.org/licenses/LICENSE-2.0

package xform

import (
	"math"
	"math/rand"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/cat"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/ordering"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/props/physical"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
	"golang.org/x/tools/container/intsets"
)

// Coster is used by the optimizer to assign a cost to a candidate expression
// that can provide a set of required physical properties. If a candidate
// expression has a lower cost than any other expression in the memo group, then
// it becomes the new best expression for the group.
//
// The set of costing formulas maintained by the coster for the set of all
// operators constitute the "cost model". A given cost model can be designed to
// maximize any optimization goal, such as:
//
//   1. Max aggregate cluster throughput (txns/sec across cluster)
//   2. Min transaction latency (time to commit txns)
//   3. Min latency to first row (time to get first row of txns)
//   4. Min memory usage
//   5. Some weighted combination of #1 - #4
//
// The cost model in this file targets #1 as the optimization goal. However,
// note that #2 is implicitly important to that goal, since overall cluster
// throughput will suffer if there are lots of pending transactions waiting on
// I/O.
//
// Coster is an interface so that different costing algorithms can be used by
// the optimizer. For example, the OptSteps command uses a custom coster that
// assigns infinite costs to some expressions in order to prevent them from
// being part of the lowest cost tree (for debugging purposes).
type Coster interface {
	// ComputeCost returns the estimated cost of executing the candidate
	// expression. The optimizer does not expect the cost to correspond to any
	// real-world metric, but does expect costs to be comparable to one another,
	// as well as summable.
	ComputeCost(candidate memo.RelExpr, required *physical.Required) memo.Cost
}

// coster encapsulates the default cost model for the optimizer. The coster
// assigns an estimated cost to each expression in the memo so that the
// optimizer can choose the lowest cost expression tree. The estimated cost is
// a best-effort approximation of the actual cost of execution, based on table
// and index statistics that are propagated throughout the logical expression
// tree.
type coster struct {
	mem *memo.Memo

	// locality gives the location of the current node as a set of user-defined
	// key/value pairs, ordered from most inclusive to least inclusive. If there
	// are no tiers, then the node's location is not known. Example:
	//
	//   [region=us,dc=east]
	//
	locality roachpb.Locality

	// perturbation indicates how much to randomly perturb the cost. It is used
	// to generate alternative plans for testing. For example, if perturbation is
	// 0.5, and the estimated cost of an expression is c, the cost returned by
	// ComputeCost will be in the range [c - 0.5 * c, c + 0.5 * c).
	perturbation float64
}

var _ Coster = &coster{}

// MakeDefaultCoster creates an instance of the default coster.
func MakeDefaultCoster(mem *memo.Memo) Coster {
	return &coster{mem: mem}
}

const (
	// These costs have been copied from the Postgres optimizer:
	// https://github.com/postgres/postgres/blob/master/src/include/optimizer/cost.h
	// TODO(rytaft): "How Good are Query Optimizers, Really?" says that the
	// PostgreSQL ratio between CPU and I/O is probably unrealistic in modern
	// systems since much of the data can be cached in memory. Consider
	// increasing the cpuCostFactor to account for this.
	cpuCostFactor    = 0.01
	seqIOCostFactor  = 1
	randIOCostFactor = 4

	// TODO(justin): make this more sophisticated.
	// lookupJoinRetrieveRowCost is the cost to retrieve a single row during a
	// lookup join.
	// See https://github.com/cockroachdb/cockroach/pull/35561 for the initial
	// justification for this constant.
	lookupJoinRetrieveRowCost = 2 * seqIOCostFactor

	// latencyCostFactor represents the throughput impact of doing scans on an
	// index that may be remotely located in a different locality. If latencies
	// are higher, then overall cluster throughput will suffer somewhat, as there
	// will be more queries in memory blocking on I/O. The impact on throughput
	// is expected to be relatively low, so latencyCostFactor is set to a small
	// value. However, even a low value will cause the optimizer to prefer
	// indexes that are likely to be geographically closer, if they are otherwise
	// the same cost to access.
	// TODO(andyk): Need to do analysis to figure out right value and/or to come
	// up with better way to incorporate latency into the coster.
	latencyCostFactor = cpuCostFactor

	// hugeCost is used with expressions we want to avoid; these are expressions
	// that "violate" a hint like forcing a specific index or join algorithm.
	// If the final expression has this cost or larger, it means that there was no
	// plan that could satisfy the hints.
	hugeCost memo.Cost = 1e100
)

// Init initializes a new coster structure with the given memo.
func (c *coster) Init(evalCtx *tree.EvalContext, mem *memo.Memo, perturbation float64) {
	c.mem = mem
	c.locality = evalCtx.Locality
	c.perturbation = perturbation
}

// ComputeCost calculates the estimated cost of the top-level operator in a
// candidate best expression, based on its logical properties and those of its
// children.
//
// Note: each custom function to compute the cost of an operator calculates
// the cost based on Big-O estimated complexity. Most constant factors are
// ignored for now.
func (c *coster) ComputeCost(candidate memo.RelExpr, required *physical.Required) memo.Cost {
	var cost memo.Cost
	switch candidate.Op() {
	case opt.SortOp:
		cost = c.computeSortCost(candidate.(*memo.SortExpr), required)

	case opt.ScanOp:
		cost = c.computeScanCost(candidate.(*memo.ScanExpr), required)

	case opt.VirtualScanOp:
		cost = c.computeVirtualScanCost(candidate.(*memo.VirtualScanExpr))

	case opt.SelectOp:
		cost = c.computeSelectCost(candidate.(*memo.SelectExpr))

	case opt.ProjectOp:
		cost = c.computeProjectCost(candidate.(*memo.ProjectExpr))

	case opt.ValuesOp:
		cost = c.computeValuesCost(candidate.(*memo.ValuesExpr))

	case opt.InnerJoinOp, opt.LeftJoinOp, opt.RightJoinOp, opt.FullJoinOp,
		opt.SemiJoinOp, opt.AntiJoinOp, opt.InnerJoinApplyOp, opt.LeftJoinApplyOp,
		opt.RightJoinApplyOp, opt.FullJoinApplyOp, opt.SemiJoinApplyOp, opt.AntiJoinApplyOp:
		// All join ops use hash join by default.
		cost = c.computeHashJoinCost(candidate)

	case opt.MergeJoinOp:
		cost = c.computeMergeJoinCost(candidate.(*memo.MergeJoinExpr))

	case opt.IndexJoinOp:
		cost = c.computeIndexJoinCost(candidate.(*memo.IndexJoinExpr))

	case opt.LookupJoinOp:
		cost = c.computeLookupJoinCost(candidate.(*memo.LookupJoinExpr))

	case opt.ZigzagJoinOp:
		cost = c.computeZigzagJoinCost(candidate.(*memo.ZigzagJoinExpr))

	case opt.UnionOp, opt.IntersectOp, opt.ExceptOp,
		opt.UnionAllOp, opt.IntersectAllOp, opt.ExceptAllOp:
		cost = c.computeSetCost(candidate)

	case opt.GroupByOp, opt.ScalarGroupByOp, opt.DistinctOnOp:
		cost = c.computeGroupingCost(candidate, required)

	case opt.LimitOp:
		cost = c.computeLimitCost(candidate.(*memo.LimitExpr))

	case opt.OffsetOp:
		cost = c.computeOffsetCost(candidate.(*memo.OffsetExpr))

	case opt.OrdinalityOp:
		cost = c.computeOrdinalityCost(candidate.(*memo.OrdinalityExpr))

	case opt.ProjectSetOp:
		cost = c.computeProjectSetCost(candidate.(*memo.ProjectSetExpr))

	case opt.ExplainOp:
		// Technically, the cost of an Explain operation is independent of the cost
		// of the underlying plan. However, we want to explain the plan we would get
		// without EXPLAIN, i.e. the lowest cost plan. So do nothing special to get
		// default behavior.
	}

	// Add a one-time cost for any operator, meant to reflect the cost of setting
	// up execution for the operator. This makes plans with fewer operators
	// preferable, all else being equal.
	cost += cpuCostFactor

	if !cost.Less(memo.MaxCost) {
		// Optsteps uses MaxCost to suppress nodes in the memo. When a node with
		// MaxCost is added to the memo, it can lead to an obscure crash with an
		// unknown node. We'd rather detect this early.
		panic(errors.AssertionFailedf("node %s with MaxCost added to the memo", log.Safe(candidate.Op())))
	}

	if c.perturbation != 0 {
		// Don't perturb the cost if we are forcing an index.
		if cost < hugeCost {
			// Get a random value in the range [-1.0, 1.0)
			multiplier := 2*rand.Float64() - 1

			// If perturbation is p, and the estimated cost of an expression is c,
			// the new cost is in the range [max(0, c - pc), c + pc). For example,
			// if p=1.5, the new cost is in the range [0, c + 1.5 * c).
			cost += cost * memo.Cost(c.perturbation*multiplier)
			// The cost must always be >= 0.
			if cost < 0 {
				cost = 0
			}
		}
	}

	return cost
}

func (c *coster) computeSortCost(sort *memo.SortExpr, required *physical.Required) memo.Cost {
	// We calculate a per-row cost and multiply by (1 + log2(rowCount)).
	// The constant term is necessary for cases where the estimated row count is
	// very small.
	// TODO(rytaft): This is the cost of a local, in-memory sort. When a
	// certain amount of memory is used, distsql switches to a disk-based sort
	// with a temp RocksDB store.
	rowCount := sort.Relational().Stats.RowCount
	perRowCost := c.rowSortCost(len(required.Ordering.Columns))
	cost := memo.Cost(rowCount) * perRowCost
	if rowCount > 1 {
		cost *= (1 + memo.Cost(math.Log2(rowCount)))
	}

	return cost
}

func (c *coster) computeScanCost(scan *memo.ScanExpr, required *physical.Required) memo.Cost {
	// Scanning an index with a few columns is faster than scanning an index with
	// many columns. Ideally, we would want to use statistics about the size of
	// each column. In lieu of that, use the number of columns.
	if scan.Flags.ForceIndex && scan.Flags.Index != scan.Index {
		// If we are forcing an index, any other index has a very high cost. In
		// practice, this will only happen when this is a primary index scan.
		return hugeCost
	}
	rowCount := scan.Relational().Stats.RowCount
	perRowCost := c.rowScanCost(scan.Table, scan.Index, scan.Cols.Len())

	if ordering.ScanIsReverse(scan, &required.Ordering) {
		if rowCount > 1 {
			// Need to do binary search to seek to the previous row.
			perRowCost += memo.Cost(math.Log2(rowCount)) * cpuCostFactor
		}
	}

	// Add a small cost if the scan is unconstrained, so all else being equal, we
	// will prefer a constrained scan. This is important if our row count
	// estimate turns out to be smaller than the actual row count.
	var preferConstrainedScanCost memo.Cost
	if scan.Constraint == nil || scan.Constraint.IsUnconstrained() {
		preferConstrainedScanCost = cpuCostFactor
	}
	return memo.Cost(rowCount)*(seqIOCostFactor+perRowCost) + preferConstrainedScanCost
}

func (c *coster) computeVirtualScanCost(scan *memo.VirtualScanExpr) memo.Cost {
	// Virtual tables are generated on-the-fly according to system metadata that
	// is assumed to be in memory.
	rowCount := memo.Cost(scan.Relational().Stats.RowCount)
	return rowCount * cpuCostFactor
}

func (c *coster) computeSelectCost(sel *memo.SelectExpr) memo.Cost {
	// The filter has to be evaluated on each input row.
	inputRowCount := sel.Input.Relational().Stats.RowCount
	cost := memo.Cost(inputRowCount) * cpuCostFactor
	return cost
}

func (c *coster) computeProjectCost(prj *memo.ProjectExpr) memo.Cost {
	// Each synthesized column causes an expression to be evaluated on each row.
	rowCount := prj.Relational().Stats.RowCount
	synthesizedColCount := len(prj.Projections)
	cost := memo.Cost(rowCount) * memo.Cost(synthesizedColCount) * cpuCostFactor

	// Add the CPU cost of emitting the rows.
	cost += memo.Cost(rowCount) * cpuCostFactor
	return cost
}

func (c *coster) computeValuesCost(values *memo.ValuesExpr) memo.Cost {
	return memo.Cost(values.Relational().Stats.RowCount) * cpuCostFactor
}

func (c *coster) computeHashJoinCost(join memo.RelExpr) memo.Cost {
	if join.Private().(*memo.JoinPrivate).Flags.DisallowHashJoin {
		return hugeCost
	}
	leftRowCount := join.Child(0).(memo.RelExpr).Relational().Stats.RowCount
	rightRowCount := join.Child(1).(memo.RelExpr).Relational().Stats.RowCount

	// A hash join must process every row from both tables once.
	//
	// We add some factors to account for the hashtable build and lookups. The
	// right side is the one stored in the hashtable, so we use a larger factor
	// for that side. This ensures that a join with the smaller right side is
	// preferred to the symmetric join.
	//
	// TODO(rytaft): This is the cost of an in-memory hash join. When a certain
	// amount of memory is used, distsql switches to a disk-based hash join with
	// a temp RocksDB store.
	cost := memo.Cost(1.25*leftRowCount+1.75*rightRowCount) * cpuCostFactor

	// Add the CPU cost of emitting the rows.
	// TODO(radu): ideally we would have an estimate of how many rows we actually
	// have to run the ON condition on.
	cost += memo.Cost(join.Relational().Stats.RowCount) * cpuCostFactor
	return cost
}

func (c *coster) computeMergeJoinCost(join *memo.MergeJoinExpr) memo.Cost {
	leftRowCount := join.Left.Relational().Stats.RowCount
	rightRowCount := join.Right.Relational().Stats.RowCount

	cost := memo.Cost(leftRowCount+rightRowCount) * cpuCostFactor

	// Add the CPU cost of emitting the rows.
	// TODO(radu): ideally we would have an estimate of how many rows we actually
	// have to run the ON condition on.
	cost += memo.Cost(join.Relational().Stats.RowCount) * cpuCostFactor
	return cost
}

func (c *coster) computeIndexJoinCost(join *memo.IndexJoinExpr) memo.Cost {
	leftRowCount := join.Input.Relational().Stats.RowCount

	// The rows in the (left) input are used to probe into the (right) table.
	// Since the matching rows in the table may not all be in the same range, this
	// counts as random I/O.
	perRowCost := cpuCostFactor + randIOCostFactor +
		c.rowScanCost(join.Table, cat.PrimaryIndex, join.Cols.Len())
	return memo.Cost(leftRowCount) * perRowCost
}

func (c *coster) computeLookupJoinCost(join *memo.LookupJoinExpr) memo.Cost {
	leftRowCount := join.Input.Relational().Stats.RowCount

	// The rows in the (left) input are used to probe into the (right) table.
	// Since the matching rows in the table may not all be in the same range, this
	// counts as random I/O.
	perLookupCost := memo.Cost(randIOCostFactor)
	cost := memo.Cost(leftRowCount) * perLookupCost

	// Each lookup might retrieve many rows; add the IO cost of retrieving the
	// rows (relevant when we expect many resulting rows per lookup) and the CPU
	// cost of emitting the rows.
	numLookupCols := join.Cols.Difference(join.Input.Relational().OutputCols).Len()
	perRowCost := lookupJoinRetrieveRowCost +
		c.rowScanCost(join.Table, join.Index, numLookupCols)

	// Add a cost if we have to evaluate an ON condition on every row. The more
	// leftover conditions, the more expensive it should be. We want to
	// differentiate between two lookup joins where one uses only a subset of the
	// columns. For example:
	//   abc JOIN xyz ON a=x AND b=y
	// We could have a lookup join using an index on y (and left-over condition
	// a=x), and another lookup join on an index on x,y. The latter is definitely
	// preferable (the former could generate a lot of internal results that are
	// then discarded).
	//
	// TODO(radu): we should take into account that the "internal" row count is
	// higher, according to the selectivities of the conditions. Unfortunately
	// this is very tricky, in particular because of left-over conditions that are
	// not selective.
	// For example:
	//   ab JOIN xy ON a=x AND x=10
	// becomes (during normalization):
	//   ab JOIN xy ON a=x AND a=10 AND x=10
	// which can become a lookup join with left-over condition x=10 which doesn't
	// actually filter anything.
	//
	// TODO(radu): this should be extended to all join types. It's tricky for hash
	// joins where we don't have the equality and leftover filters readily
	// available.
	perRowCost += cpuCostFactor * memo.Cost(len(join.On))

	cost += memo.Cost(join.Relational().Stats.RowCount) * perRowCost
	return cost
}

func (c *coster) computeZigzagJoinCost(join *memo.ZigzagJoinExpr) memo.Cost {
	rowCount := join.Relational().Stats.RowCount

	// Assume the upper bound on scan cost to be the sum of the cost of
	// scanning the two constituent indexes. To determine how many columns
	// are returned from each scan, intersect the output column set join.Cols
	// with each side's IndexColumns. Columns present in both indexes are
	// projected from the left side only.
	md := c.mem.Metadata()
	leftCols := md.TableMeta(join.LeftTable).IndexColumns(join.LeftIndex)
	leftCols.IntersectionWith(join.Cols)
	rightCols := md.TableMeta(join.RightTable).IndexColumns(join.RightIndex)
	rightCols.IntersectionWith(join.Cols)
	rightCols.DifferenceWith(leftCols)
	scanCost := c.rowScanCost(join.LeftTable, join.LeftIndex, leftCols.Len())
	scanCost += c.rowScanCost(join.RightTable, join.RightIndex, rightCols.Len())

	// Double the cost of emitting rows as well as the cost of seeking rows,
	// given two indexes will be accessed.
	cost := memo.Cost(rowCount) * (2*(cpuCostFactor+seqIOCostFactor) + scanCost)
	return cost
}

func (c *coster) computeSetCost(set memo.RelExpr) memo.Cost {
	// Add the CPU cost of emitting the rows.
	cost := memo.Cost(set.Relational().Stats.RowCount) * cpuCostFactor

	// A set operation must process every row from both tables once.
	// UnionAll can avoid any extra computation, but all other set operations
	// must perform a hash table lookup or update for each input row.
	if set.Op() != opt.UnionAllOp {
		leftRowCount := set.Child(0).(memo.RelExpr).Relational().Stats.RowCount
		rightRowCount := set.Child(1).(memo.RelExpr).Relational().Stats.RowCount
		cost += memo.Cost(leftRowCount+rightRowCount) * cpuCostFactor
	}

	return cost
}

func (c *coster) computeGroupingCost(grouping memo.RelExpr, required *physical.Required) memo.Cost {
	// Add the CPU cost of emitting the rows.
	cost := memo.Cost(grouping.Relational().Stats.RowCount) * cpuCostFactor

	// GroupBy must process each input row once. Cost per row depends on the
	// number of grouping columns and the number of aggregates.
	inputRowCount := grouping.Child(0).(memo.RelExpr).Relational().Stats.RowCount
	aggsCount := grouping.Child(1).ChildCount()
	private := grouping.Private().(*memo.GroupingPrivate)
	groupingColCount := private.GroupingCols.Len()
	cost += memo.Cost(inputRowCount) * memo.Cost(aggsCount+groupingColCount) * cpuCostFactor

	if groupingColCount > 0 {
		// Add a cost that reflects the use of a hash table - unless we are doing a
		// streaming aggregation where all the grouping columns are ordered; we
		// interpolate linearly if only part of the grouping columns are ordered.
		//
		// The cost is chosen so that it's always less than the cost to sort the
		// input.
		hashCost := memo.Cost(inputRowCount) * cpuCostFactor
		n := len(ordering.StreamingGroupingColOrdering(private, &required.Ordering))
		// n = 0:                factor = 1
		// n = groupingColCount: factor = 0
		hashCost *= 1 - memo.Cost(n)/memo.Cost(groupingColCount)
		cost += hashCost
	}

	return cost
}

func (c *coster) computeLimitCost(limit *memo.LimitExpr) memo.Cost {
	// Add the CPU cost of emitting the rows.
	cost := memo.Cost(limit.Relational().Stats.RowCount) * cpuCostFactor
	return cost
}

func (c *coster) computeOffsetCost(offset *memo.OffsetExpr) memo.Cost {
	// Add the CPU cost of emitting the rows.
	cost := memo.Cost(offset.Relational().Stats.RowCount) * cpuCostFactor
	return cost
}

func (c *coster) computeOrdinalityCost(ord *memo.OrdinalityExpr) memo.Cost {
	// Add the CPU cost of emitting the rows.
	cost := memo.Cost(ord.Relational().Stats.RowCount) * cpuCostFactor
	return cost
}

func (c *coster) computeProjectSetCost(projectSet *memo.ProjectSetExpr) memo.Cost {
	// Add the CPU cost of emitting the rows.
	cost := memo.Cost(projectSet.Relational().Stats.RowCount) * cpuCostFactor
	return cost
}

// rowSortCost is the CPU cost to sort one row, which depends on the number of
// columns in the sort key.
func (c *coster) rowSortCost(numKeyCols int) memo.Cost {
	// Sorting involves comparisons on the key columns, but the cost isn't
	// directly proportional: we only compare the second column if the rows are
	// equal on the first column; and so on. We also account for a fixed
	// "non-comparison" cost related to processing the
	// row. The formula is:
	//
	//   cpuCostFactor * [ 1 + Sum eqProb^(i-1) with i=1 to numKeyCols ]
	//
	const eqProb = 0.1
	cost := cpuCostFactor
	for i, c := 0, cpuCostFactor; i < numKeyCols; i, c = i+1, c*eqProb {
		// c is cpuCostFactor * eqProb^i.
		cost += c
	}

	// There is a fixed "non-comparison" cost and a comparison cost proportional
	// to the key columns. Note that the cost has to be high enough so that a
	// sort is almost always more expensive than a reverse scan or an index scan.
	return memo.Cost(cost)
}

// rowScanCost is the CPU cost to scan one row, which depends on the number of
// columns in the index and (to a lesser extent) on the number of columns we are
// scanning.
func (c *coster) rowScanCost(tabID opt.TableID, idxOrd int, numScannedCols int) memo.Cost {
	md := c.mem.Metadata()
	tab := md.Table(tabID)
	idx := tab.Index(idxOrd)
	numCols := idx.ColumnCount()

	// Adjust cost based on how well the current locality matches the index's
	// zone constraints.
	var costFactor memo.Cost = cpuCostFactor
	if len(c.locality.Tiers) != 0 {
		// If 0% of locality tiers have matching constraints, then add additional
		// cost. If 100% of locality tiers have matching constraints, then add no
		// additional cost. Anything in between is proportional to the number of
		// matches.
		adjustment := 1.0 - localityMatchScore(idx.Zone(), c.locality)
		costFactor += latencyCostFactor * memo.Cost(adjustment)
	}

	// The number of the columns in the index matter because more columns means
	// more data to scan. The number of columns we actually return also matters
	// because that is the amount of data that we could potentially transfer over
	// the network.
	return memo.Cost(numCols+numScannedCols) * costFactor
}

// localityMatchScore returns a number from 0.0 to 1.0 that describes how well
// the current node's locality matches the given zone constraints and
// leaseholder preferences, with 0.0 indicating 0% and 1.0 indicating 100%. This
// is the basic algorithm:
//
//   t = total # of locality tiers
//
//   Match each locality tier against the constraint set, and compute a value
//   for each tier:
//
//      0 = key not present in constraint set or key matches prohibited
//          constraint, but value doesn't match
//     +1 = key matches required constraint, and value does match
//     -1 = otherwise
//
//   m = length of longest locality prefix that ends in a +1 value and doesn't
//       contain a -1 value.
//
//   Compute "m" for both the ReplicaConstraints constraints set, as well as for
//   the LeasePreferences constraints set:
//
//     constraint-score = m / t
//     lease-pref-score = m / t
//
//   if there are no lease preferences, then final-score = lease-pref-score
//   else final-score = (constraint-score * 2 + lease-pref-score) / 3
//
// Here are some scoring examples:
//
//   Locality = region=us,dc=east
//   0.0 = []                     // No constraints to match
//   0.0 = [+region=eu,+dc=uk]    // None of the tiers match
//   0.0 = [+region=eu,+dc=east]  // 2nd tier matches, but 1st tier doesn't
//   0.0 = [-region=us,+dc=east]  // 1st tier matches PROHIBITED constraint
//   0.0 = [-region=eu]           // 1st tier PROHIBITED and non-matching
//   0.5 = [+region=us]           // 1st tier matches
//   0.5 = [+region=us,-dc=east]  // 1st tier matches, 2nd tier PROHIBITED
//   0.5 = [+region=us,+dc=west]  // 1st tier matches, but 2nd tier doesn't
//   1.0 = [+region=us,+dc=east]  // Both tiers match
//   1.0 = [+dc=east]             // 2nd tier matches, no constraints for 1st
//   1.0 = [+region=us,+dc=east,+rack=1,-ssd]  // Extra constraints ignored
//
// Note that constraints need not be specified in any particular order, so all
// constraints are scanned when matching each locality tier. In cases where
// there are multiple replica constraint groups (i.e. where a subset of replicas
// can have different constraints than another subset), the minimum constraint
// score among the groups is used.
//
// While matching leaseholder preferences are considered in the final score,
// leaseholder preferences are not guaranteed, so its score is weighted at half
// of the replica constraint score, in order to reflect the possibility that the
// leaseholder has moved from the preferred location.
func localityMatchScore(zone cat.Zone, locality roachpb.Locality) float64 {
	// Fast path: if there are no constraints or leaseholder preferences, then
	// locality can't match.
	if zone.ReplicaConstraintsCount() == 0 && zone.LeasePreferenceCount() == 0 {
		return 0.0
	}

	// matchTier matches a tier to a set of constraints and returns:
	//
	//    0 = key not present in constraint set or key only matches prohibited
	//        constraints where value doesn't match
	//   +1 = key matches any required constraint key + value
	//   -1 = otherwise
	//
	matchTier := func(tier roachpb.Tier, set cat.ConstraintSet) int {
		foundNoMatch := false
		for j, n := 0, set.ConstraintCount(); j < n; j++ {
			con := set.Constraint(j)
			if con.GetKey() != tier.Key {
				// Ignore constraints that don't have matching key.
				continue
			}

			if con.GetValue() == tier.Value {
				if !con.IsRequired() {
					// Matching prohibited constraint, so result is -1.
					return -1
				}

				// Matching required constraint, so result is +1.
				return +1
			}

			if con.IsRequired() {
				// Remember that non-matching required constraint was found.
				foundNoMatch = true
			}
		}

		if foundNoMatch {
			// At least one non-matching required constraint was found, and no
			// matching constraints.
			return -1
		}

		// Key not present in constraint set, or key only matches prohibited
		// constraints where value doesn't match.
		return 0
	}

	// matchConstraints returns the number of tiers that match the given
	// constraint set ("m" in algorithm described above).
	matchConstraints := func(set cat.ConstraintSet) int {
		matchCount := 0
		for i, tier := range locality.Tiers {
			switch matchTier(tier, set) {
			case +1:
				matchCount = i + 1
			case -1:
				return matchCount
			}
		}
		return matchCount
	}

	// Score any replica constraints.
	var constraintScore float64
	if zone.ReplicaConstraintsCount() != 0 {
		// Iterate over the replica constraints and determine the minimum value
		// returned by matchConstraints for any replica. For example:
		//
		//   3: [+region=us,+dc=east]
		//   2: [+region=us]
		//
		// For the [region=us,dc=east] locality, the result is min(2, 1).
		minCount := intsets.MaxInt
		for i := 0; i < zone.ReplicaConstraintsCount(); i++ {
			matchCount := matchConstraints(zone.ReplicaConstraints(i))
			if matchCount < minCount {
				minCount = matchCount
			}
		}

		constraintScore = float64(minCount) / float64(len(locality.Tiers))
	}

	// If there are no lease preferences, then use replica constraint score.
	if zone.LeasePreferenceCount() == 0 {
		return constraintScore
	}

	// Score the first lease preference, if one is available. Ignore subsequent
	// lease preferences, since they only apply in edge cases.
	matchCount := matchConstraints(zone.LeasePreference(0))
	leaseScore := float64(matchCount) / float64(len(locality.Tiers))

	// Weight the constraintScore twice as much as the lease score.
	return (constraintScore*2 + leaseScore) / 3
}
