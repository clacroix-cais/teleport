// Copyright 2022 Gravitational, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package predicate

const (
	dpllSatisfied = iota
	dpllUnsatisfied
	dpllUnknown
)

type node any

type nodeLiteral struct {
	value bool
}

type nodeIdentifier struct {
	key string
}

type nodeNot struct {
	left node
}

type nodeOr struct {
	left  node
	right node
}

type nodeAnd struct {
	left  node
	right node
}

type assignment struct {
	key   string
	value bool
}

type state struct {
	clauses     []node
	assignments []assignment
	watched     map[string][]node
	enforce     []node
	uprop       []node
}

func newState(clause node) *state {
	// todo: cnf conversion
	clauses := []node{clause}
	watched := make(map[string][]node)
	enforce := make([]node, 0)
	uprop := make([]node, 0)

	for _, clause := range clauses {
		a := pickLiteral(clause, func(x string) bool { return false })
		if a != nil {
			enforce = append(enforce, clause)
			continue
		}

		b := pickLiteral(clause, func(x string) bool { return x != *a })
		if b != nil {
			uprop = append(uprop, clause)
			continue
		}

		for _, lit := range []string{*a, *b} {
			watched[lit] = append(watched[lit], clause)
		}
	}

	return &state{
		clauses: clauses,
		watched: watched,
		enforce: enforce,
		uprop:   uprop,
	}
}

func pickLiteral(node node, isExcluded func(string) bool) *string {
	or := func(left, right *string) *string {
		if left != nil {
			return left
		}

		return right
	}

	switch node := node.(type) {
	case *nodeLiteral:
		return nil
	case *nodeIdentifier:
		if isExcluded(node.key) {
			return nil
		}

		return &node.key
	case *nodeNot:
		return pickLiteral(node.left, isExcluded)
	case *nodeOr:
		return or(pickLiteral(node.left, isExcluded), pickLiteral(node.right, isExcluded))
	case *nodeAnd:
		return or(pickLiteral(node.left, isExcluded), pickLiteral(node.right, isExcluded))
	default:
		panic("unreachable")
	}
}

func evalClause(state *state, clause node) int {
	return dpllUnknown
}

func isAssigned(state *state, key string) (bool, bool) {
	for _, assignment := range state.assignments {
		if assignment.key == key {
			return true, assignment.value
		}
	}

	return false, false
}

func pickUnassigned(state *state) *string {
	for _, clause := range state.clauses {
		a := pickLiteral(clause, func(x string) bool { assigned, _ := isAssigned(state, x); return assigned })
		if a != nil {
			return a
		}
	}

	return nil
}

func backtrackAdjust(state *state, rem []assignment) bool {
	satisfied := func() bool {
		for _, clause := range state.clauses {
			switch evalClause(state, clause) {
			case dpllSatisfied, dpllUnknown:
				continue
			case dpllUnsatisfied:
				return false
			}
		}

		return true
	}

	recurse := func() bool { return satisfied() || backtrackAdjust(state, rem[1:]) }

	if recurse() {
		return true
	}

	if len(rem) > 0 {
		ass := &rem[len(rem)-1]
		ass.value = !ass.value

		if recurse() {
			return true
		}
	}

	return false
}

// opt: watch-literal based unit propagation
func dpll(state *state) bool {
	// check that nonvariable clauses are satisfied
	for _, clause := range state.enforce {
		switch evalClause(state, clause) {
		case dpllSatisfied:
			continue
		case dpllUnsatisfied:
			// pure clause cannot be satisfied, formula is unsat
			return false
		case dpllUnknown:
			panic("unreachable")
		}
	}

	for {
		literal := pickUnassigned(state)
		if literal == nil {
			// all variables are assigned, formula is sat
			return true
		}

		state.assignments = append(state.assignments, assignment{key: *literal, value: true})
		if !backtrackAdjust(state, state.assignments) {
			// backtrack failed, formula is unsat
			return false
		}
	}
}
