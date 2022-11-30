/*
Copyright 2022 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package predicate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"time"

	"github.com/aclements/go-z3/z3"
	"github.com/gravitational/trace"
)

// Resolver resolves an identifier to a value. Returns nil if the identifier is not found and is unknown.
type Resolver func([]string) any

// Type represents a type of a value within a predicate expression.
type Type int

const (
	// TypeInt is an Go int type.
	TypeInt Type = iota

	// TypeBool is a Go bool type.
	TypeBool

	// TypeString is a Go string type.
	TypeString
)

// Solver is a solver instance which can be used to solve partial predicate expressions.
// This is rather expensive to create so it should preferably be reused when possible.
type Solver struct {
	def    *z3.Context
	solver *z3.Solver
}

// NewSolver creates a new solver instance.
func NewSolver() *Solver {
	config := z3.NewContextConfig()
	def := z3.NewContext(config)
	solver := z3.NewSolver(def)
	return &Solver{def, solver}
}

// PartialSolveForAll solves a given partial predicate expression for all possible values of the given identifier.
// The expression must have a boolean output type and the type of the unknown identifier must be specified.
// On timeout, no solutions are returned.
func (s *Solver) PartialSolveForAll(predicate string, resolveIdentifier Resolver, querying string, to Type, timeout time.Duration) ([]z3.Value, error) {
	outCh := make(chan []z3.Value, 1)
	errCh := make(chan error, 1)

	// Run the solver in a separate goroutine to allow the caller to handle timeouts.
	go func() {
		defer close(outCh)
		defer close(errCh)

		out, err := s.partialSolveForAllImpl(predicate, resolveIdentifier, querying, to)
		if err != nil {
			errCh <- err
			return
		}

		outCh <- out
	}()

	select {
	case out := <-outCh:
		return out, nil
	case err := <-errCh:
		return nil, err
	case <-time.After(timeout):
		s.def.Interrupt()
		// wait for the other goroutine to cleanup
		<-outCh
		return nil, trace.LimitExceeded("timeout")
	}
}

// partialSolveForAllImpl is the implementation of PartialSolveForAll that runs logic directly.
func (s *Solver) partialSolveForAllImpl(predicate string, resolveIdentifier Resolver, querying string, to Type) ([]z3.Value, error) {
	// parse the predicate expression into a Go AST
	ast, err := parser.ParseExpr(predicate)
	if err != nil {
		return nil, err
	}

	// create a context struct which is used to share various information obtained during lowering
	ctx := &ctx{s.def, s.solver, make(map[string]z3.Value), z3.Sort{}, resolveIdentifier}

	// reset the state of the Z3 solver on exit so that it can be reused
	defer ctx.solver.Reset()

	// create a Z3 identifier for the unknown value
	switch to {
	case TypeInt:
		ctx.unkTy = s.def.IntSort()
	case TypeBool:
		ctx.unkTy = s.def.BoolSort()
	case TypeString:
		ctx.unkTy = s.def.StringSort()
	default:
		return nil, trace.BadParameter("unsupported output type %v", to)
	}

	// lower the Go AST into a Z3 AST
	cond, err := lower(ctx, ast)
	if err != nil {
		return nil, err
	}

	// assert that the expression must evaluate to true
	ctx.solver.Assert(cond.(z3.Bool))
	var out []z3.Value

	// retrieve all possible values for the unknown identifier
	for {
		// solve the model
		sat, err := ctx.solver.Check()
		if err != nil {
			return nil, err
		}

		// if the model is unsatisfiable, we are done
		if !sat {
			return out, nil
		}

		// retrieve the model containing the output value
		model := ctx.solver.Model()

		// retrieve the value of the unknown identifier, this will error if the identifier is not found within the expression
		val, ok := ctx.idents[querying]
		if !ok {
			return nil, trace.NotFound("identifier %v not found", querying)
		}

		// retrieve the value of the unknown identifier from the model
		last := model.Eval(val, true)
		out = append(out, last)

		// assert that the value of the next possible is not equal to the current value, this allows us to retrieve all possible values
		neq := ctx.def.Distinct(val, last)
		ctx.solver.Assert(neq)
	}
}

type ctx struct {
	def               *z3.Context
	solver            *z3.Solver
	idents            map[string]z3.Value
	unkTy             z3.Sort
	resolveIdentifier Resolver
}

func (ctx *ctx) resolve(fields []string) (z3.Value, error) {
	full := strings.Join(fields, ".")
	if v, ok := ctx.idents[full]; ok {
		return v, nil
	}

	if val := ctx.resolveIdentifier(fields); val != nil {
		var ident z3.Value
		switch val := val.(type) {
		case int:
			ident = ctx.def.FromInt(int64(val), ctx.def.IntSort())
		case string:
			ident = ctx.def.FromString(val)
		default:
			return nil, trace.BadParameter("unsupported type %T", val)
		}

		ctx.idents[full] = ident
		return ident, nil
	}

	val := ctx.def.Const(full, ctx.unkTy)
	ctx.idents[full] = val
	return val, nil
}

func lower(ctx *ctx, node ast.Expr) (z3.Value, error) {
	switch n := node.(type) {
	case *ast.ParenExpr:
		return lower(ctx, n.X)
	case *ast.BinaryExpr:
		return lowerBinary(ctx, n)
	case *ast.UnaryExpr:
		return lowerUnary(ctx, n)
	case *ast.BasicLit:
		return lowerBasicLit(ctx, n)
	case *ast.IndexExpr:
		return lowerIndexExpr(ctx, n)
	case *ast.SelectorExpr:
		return lowerSelectorExpr(ctx, n)
	case *ast.Ident:
		return lowerIdent(ctx, n)
	case *ast.CallExpr:
		return lowerCallExpr(ctx, n)
	default:
		return nil, trace.NotImplemented("node type %T unsupported", n)
	}
}

func pickType(ctx *ctx, x, y z3.Value) (z3.Kind, error) {
	xk := x.Sort().Kind()
	yk := y.Sort().Kind()
	if xk != yk {
		return 0, trace.BadParameter("type mismatch %v != %v", xk, yk)
	}

	return xk, nil
}

func lowerBinary(ctx *ctx, node *ast.BinaryExpr) (z3.Value, error) {
	x, err := lower(ctx, node.X)
	if err != nil {
		return nil, err
	}

	y, err := lower(ctx, node.Y)
	if err != nil {
		return nil, err
	}

	switch node.Op {
	case token.EQL:
		ty, err := pickType(ctx, x, y)
		if err != nil {
			return nil, err
		}

		switch ty {
		case z3.KindInt:
			return x.(z3.Int).Eq(y.(z3.Int)), nil
		case z3.KindBool:
			return x.(z3.Bool).Eq(y.(z3.Bool)), nil
		case z3.KindString:
			return x.(z3.String).Eq(y.(z3.String)), nil
		default:
			return nil, trace.BadParameter("type %v does not support equals", ty)
		}
	case token.LSS:
		xi := x.(z3.Int)
		yi := y.(z3.Int)
		return xi.LT(yi), nil
	case token.LEQ:
		xi := x.(z3.Int)
		yi := y.(z3.Int)
		return xi.LE(yi), nil
	case token.GTR:
		xi := x.(z3.Int)
		yi := y.(z3.Int)
		return xi.GT(yi), nil
	case token.GEQ:
		xi := x.(z3.Int)
		yi := y.(z3.Int)
		return xi.GE(yi), nil
	case token.LAND:
		xb := x.(z3.Bool)
		yb := y.(z3.Bool)
		return xb.And(yb), nil
	case token.LOR:
		xb := x.(z3.Bool)
		yb := y.(z3.Bool)
		return xb.Or(yb), nil
	default:
		return nil, trace.NotImplemented("unary op %v unsupported", node.Op)
	}
}

func lowerUnary(ctx *ctx, node *ast.UnaryExpr) (z3.Value, error) {
	switch node.Op {
	case token.NOT:
		x, err := lower(ctx, node.X)
		if err != nil {
			return nil, err
		}

		return x.(z3.Bool).Not(), nil
	default:
		return nil, trace.NotImplemented("unary op %v unsupported", node.Op)
	}
}

func lowerBasicLit(ctx *ctx, node *ast.BasicLit) (z3.Value, error) {
	switch {
	case node.Kind == token.INT:
		value, err := strconv.Atoi(node.Value)
		if err != nil {
			return nil, err
		}

		return ctx.def.FromInt(int64(value), ctx.def.IntSort()), nil
	case node.Kind == token.STRING:
		return ctx.def.FromString(node.Value[1 : len(node.Value)-1]), nil
	default:
		return nil, trace.NotImplemented("basic lit kind %v unsupported", node.Kind)
	}
}

func lowerIndexExpr(ctx *ctx, node *ast.IndexExpr) (z3.Value, error) {
	// todo: impl maps
	return nil, trace.BadParameter("maps are not supported")
}

func lowerSelectorExpr(ctx *ctx, node *ast.SelectorExpr) (z3.Value, error) {
	fields, err := evaluateSelector(node, []string{})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return ctx.resolve(fields)
}

func evaluateSelector(sel *ast.SelectorExpr, fields []string) ([]string, error) {
	fields = append([]string{sel.Sel.Name}, fields...)
	switch l := sel.X.(type) {
	case *ast.SelectorExpr:
		return evaluateSelector(l, fields)
	case *ast.Ident:
		fields = append([]string{l.Name}, fields...)
		return fields, nil
	default:
		return nil, trace.BadParameter("unsupported selector type: %T", l)
	}
}

func lowerIdent(ctx *ctx, node *ast.Ident) (z3.Value, error) {
	switch node.Name {
	case "true":
		return ctx.def.FromBool(true), nil
	case "false":
		return ctx.def.FromBool(true), nil
	default:
		return ctx.resolve([]string{node.Name})
	}
}

func lowerCallExpr(ctx *ctx, node *ast.CallExpr) (z3.Value, error) {
	// todo: impl fn calls
	return nil, trace.BadParameter("fn calls are not supported")
}