package query

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/influxdata/influxdb/influxql"
)

// stuff that needs to be known globally
// global state, which is changed by each field that gets compiled.
// global constants or configuration which never changes. might not be worth separating those.
// a list of the function calls that happen.
// whether or not auxiliary fields are needed.

// the final field that is constructed after linking
type Field struct {
	Name string // the resolved name of the field
}

type CompileOptions struct {
	Now time.Time
}

type compiledStatement struct {
	// Sources holds the data sources this will query from.
	Sources influxql.Sources

	// Dimensions holds the groupings for the statement.
	Dimensions []string

	// Interval holds the time grouping interval.
	Interval influxql.Interval

	// FunctionCalls holds a reference to the read edge of all of the
	// function calls that have been instantiated.
	FunctionCalls []*ReadEdge

	// OnlySelectors is set to true when there are no aggregate functions.
	OnlySelectors bool

	// TopBottomFunction is set to top or bottom when one of those functions are
	// used in the statement.
	TopBottomFunction string

	// AuxiliaryFields holds a mapping to the auxiliary fields that need to be
	// selected. This maps the raw VarRef to a pointer to a shared VarRef. The
	// pointer is used for instantiating references to the shared variable so
	// type mapping gets shared.
	AuxiliaryFields *AuxiliaryFields

	// Fields holds all of the compiled fields that will be used.
	Fields []*compiledField

	// Options holds the configured compiler options.
	Options CompileOptions
}

type CompiledStatement interface {
	Select(plan *Plan) ([]*ReadEdge, error)
}

func newCompiler(stmt *influxql.SelectStatement, opt CompileOptions) *compiledStatement {
	if opt.Now.IsZero() {
		opt.Now = time.Now().UTC()
	}
	return &compiledStatement{
		OnlySelectors: true,
		Fields:        make([]*compiledField, 0, len(stmt.Fields)),
		Options:       opt,
	}
}

// Wildcard represents a wildcard within a field.
type wildcard struct {
	// NameFilters are the regexp filters for selecting fields. If this is
	// nil, no fields are filtered because of their name.
	NameFilters []*regexp.Regexp

	// TypeFilters holds a list of all of the types forbidden to be used
	// because of a function.
	TypeFilters map[influxql.DataType]struct{}
}

// compiledField holds the compilation state for a field.
type compiledField struct {
	// This holds the global state from the compiled statement.
	global *compiledStatement

	// Field contains the original field associated with this field.
	Field *influxql.Field

	// Output contains the output edge for this field.
	Output *ReadEdge

	// Symbols contains a list of all of the unresolved symbols within this
	// field.
	Symbols []interface{}

	// Wildcard contains the wildcard expression to be used when resolving
	// wildcards.
	Wildcard *wildcard
}

// compileExpr creates the node that executes the expression and connects that
// node to the WriteEdge as the output.
func (c *compiledField) compileExpr(expr influxql.Expr, out *WriteEdge) error {
	switch expr := expr.(type) {
	case *influxql.VarRef:
		// A bare variable reference will require auxiliary fields.
		c.global.requireAuxiliaryFields()
		// Add a symbol that resolves to this write edge.
		// TODO(jsternberg): Add symbol resolution.
		return nil
	case *influxql.Wildcard:
		// Wildcards use auxiliary fields. We assume there will be at least one
		// expansion.
		c.global.requireAuxiliaryFields()
		c.wildcard()
	case *influxql.RegexLiteral:
		c.global.requireAuxiliaryFields()
		c.wildcardFilter(expr.Val)
	case *influxql.Call:
		switch expr.Name {
		case "count", "min", "max", "sum", "first", "last", "mean":
			return c.compileFunction(expr, out)
		case "distinct":
			return c.compileDistinct(expr, out, false)
		case "top", "bottom":
			return c.compileTopBottom(expr, out)
		default:
			return errors.New("unimplemented")
		}
	case *influxql.Distinct:
		return c.compileDistinct(expr.NewCall(), out, false)
	case *influxql.BinaryExpr:
		// Check if either side is a literal so we only compile one side if it is.
		if _, ok := expr.LHS.(influxql.Literal); ok {
		} else if _, ok := expr.RHS.(influxql.Literal); ok {
		} else {
			// Construct a binary expression and an input edge for each side.
			node := &BinaryExpr{Op: expr.Op, Output: out}
			out.Node = node

			// Process the left side.
			var lhs *WriteEdge
			lhs, node.LHS = AddEdge(nil, node)
			if err := c.compileExpr(expr.LHS, lhs); err != nil {
				return err
			}

			// Process the right side.
			var rhs *WriteEdge
			rhs, node.RHS = AddEdge(nil, node)
			if err := c.compileExpr(expr.RHS, rhs); err != nil {
				return err
			}
			return nil
		}
	}
	return errors.New("unimplemented")
}

func (c *compiledField) compileFunction(expr *influxql.Call, out *WriteEdge) error {
	if exp, got := 1, len(expr.Args); exp != got {
		return fmt.Errorf("invalid number of arguments for %s, expected %d, got %d", expr.Name, exp, got)
	}

	// Create the function call and send its output to the write edge.
	call := &FunctionCall{Name: expr.Name, Output: out}
	c.global.FunctionCalls = append(c.global.FunctionCalls, out.Output)
	out.Node = call
	out, call.Input = AddEdge(nil, call)

	// Mark down some meta properties related to the function for query validation.
	switch expr.Name {
	case "max", "min", "first", "last", "percentile", "sample":
		// top/bottom are not included here since they are not typical functions.
	default:
		c.global.OnlySelectors = false
	}

	// If this is a call to count(), allow distinct() to be used as the function argument.
	if expr.Name == "count" {
		// If we have count(), the argument may be a distinct() call.
		if arg0, ok := expr.Args[0].(*influxql.Call); ok && arg0.Name == "distinct" {
			return c.compileDistinct(arg0, out, true)
		} else if arg0, ok := expr.Args[0].(*influxql.Distinct); ok {
			return c.compileDistinct(arg0.NewCall(), out, true)
		}
	}

	// Must be a variable reference, wildcard, or regexp.
	switch arg0 := expr.Args[0].(type) {
	case *influxql.VarRef:
		return c.global.compileVarRef(arg0, out)
	case *influxql.Wildcard:
		c.wildcardFunction(expr.Name)
		return nil
	case *influxql.RegexLiteral:
		c.wildcardFunctionFilter(expr.Name, arg0.Val)
		return nil
	default:
		return fmt.Errorf("expected field argument in %s()", expr.Name)
	}
}

func (c *compiledStatement) linkAuxiliaryFields() error {
	if c.AuxiliaryFields == nil {
		if len(c.FunctionCalls) == 0 {
			return errors.New("at least 1 non-time field must be queried")
		}
		return nil
	}

	if c.AuxiliaryFields != nil {
		if !c.OnlySelectors {
			return fmt.Errorf("mixing aggregate and non-aggregate queries is not supported")
		} else if len(c.FunctionCalls) > 1 {
			return fmt.Errorf("mixing multiple selector functions with tags or fields is not supported")
		}

		if len(c.FunctionCalls) == 1 {
			c.AuxiliaryFields.Input, c.AuxiliaryFields.Output = c.FunctionCalls[0].Insert(c.AuxiliaryFields)
		} else {
			// Create a default IteratorCreator for this AuxiliaryFields.
			var out *WriteEdge
			out, c.AuxiliaryFields.Input = AddEdge(nil, c.AuxiliaryFields)
			if err := c.compileVarRef(nil, out); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *compiledField) compileDistinct(call *influxql.Call, out *WriteEdge, nested bool) error {
	if len(call.Args) == 0 {
		return errors.New("distinct function requires at least one argument")
	} else if len(call.Args) != 1 {
		return errors.New("distinct function can only have one argument")
	}

	arg0, ok := call.Args[0].(*influxql.VarRef)
	if !ok {
		return errors.New("expected field argument in distinct()")
	}

	// Add the distinct node to the graph.
	d := &Distinct{Output: out}
	if !nested {
		// Add as a function call if this is not nested.
		c.global.FunctionCalls = append(c.global.FunctionCalls, out.Output)
	}
	out.Node = d
	out, d.Input = AddEdge(nil, d)

	// Add the variable reference to the graph to complete the graph.
	return c.global.compileVarRef(arg0, out)
}

func (c *compiledField) compileTopBottom(call *influxql.Call, out *WriteEdge) error {
	if c.global.TopBottomFunction != "" {
		return fmt.Errorf("selector function %s() cannot be combined with other functions", c.global.TopBottomFunction)
	}

	if exp, got := 2, len(call.Args); got < exp {
		return fmt.Errorf("invalid number of arguments for %s, expected at least %d, got %d", call.Name, exp, got)
	}

	ref, ok := call.Args[0].(*influxql.VarRef)
	if !ok {
		return fmt.Errorf("expected field argument in %s()", call.Name)
	}

	var dimensions []influxql.VarRef
	if len(call.Args) > 2 {
		dimensions = make([]influxql.VarRef, 0, len(call.Args))
		for _, v := range call.Args[1 : len(call.Args)-1] {
			if ref, ok := v.(*influxql.VarRef); ok {
				dimensions = append(dimensions, *ref)
			} else {
				return fmt.Errorf("only fields or tags are allowed in %s(), found %s", call.Name, v)
			}
		}
	}

	limit, ok := call.Args[len(call.Args)-1].(*influxql.IntegerLiteral)
	if !ok {
		return fmt.Errorf("expected integer as last argument in %s(), found %s", call.Name, call.Args[len(call.Args)-1])
	} else if limit.Val <= 0 {
		return fmt.Errorf("limit (%d) in %s function must be at least 1", limit.Val, call.Name)
	}
	c.global.TopBottomFunction = call.Name

	selector := &TopBottomSelector{Dimensions: dimensions, Output: out}
	out.Node = selector

	out, selector.Input = AddEdge(nil, selector)
	return c.global.compileVarRef(ref, out)
}

func (c *compiledField) wildcard() {
	if c.Wildcard == nil {
		c.Wildcard = &wildcard{
			TypeFilters: make(map[influxql.DataType]struct{}),
		}
	}
}

func (c *compiledField) wildcardFilter(filter *regexp.Regexp) {
	c.wildcard()
	c.Wildcard.NameFilters = append(c.Wildcard.NameFilters, filter)
}

func (c *compiledField) wildcardFunction(name string) {
	c.wildcard()
	switch name {
	default:
		c.Wildcard.TypeFilters[influxql.String] = struct{}{}
	case "count", "first", "last", "distinct", "elapsed", "mode", "sample":
		c.Wildcard.TypeFilters[influxql.Boolean] = struct{}{}
	case "min", "max":
		// No restrictions.
	}
}

func (c *compiledField) wildcardFunctionFilter(name string, filter *regexp.Regexp) {
	c.wildcardFunction(name)
	c.Wildcard.NameFilters = append(c.Wildcard.NameFilters, filter)
}

func (c *compiledStatement) compileVarRef(ref *influxql.VarRef, out *WriteEdge) error {
	merge := &Merge{Output: out}
	for _, source := range c.Sources {
		switch source := source.(type) {
		case *influxql.Measurement:
			ic := &IteratorCreator{
				Expr:            ref,
				AuxiliaryFields: &c.AuxiliaryFields,
				Measurement:     source,
			}
			ic.Output = merge.AddInput(ic)
		default:
			return errors.New("unimplemented")
		}
	}
	out.Node = merge
	return nil
}

func (c *compiledStatement) validateFields() error {
	// Ensure there are not multiple calls if top/bottom is present.
	if len(c.FunctionCalls) > 1 && c.TopBottomFunction != "" {
		return fmt.Errorf("selector function %s() cannot be combined with other functions", c.TopBottomFunction)
	}
	return nil
}

func Compile(stmt *influxql.SelectStatement, opt CompileOptions) (CompiledStatement, error) {
	// Compile each of the expressions.
	c := newCompiler(stmt, opt)
	c.Sources = append(c.Sources, stmt.Sources...)

	// Read the dimensions of the query and retrieve the interval if it exists.
	c.Dimensions = make([]string, 0, len(stmt.Dimensions))
	for _, d := range stmt.Dimensions {
		switch expr := d.Expr.(type) {
		case *influxql.VarRef:
			if strings.ToLower(expr.Val) == "time" {
				return nil, errors.New("time() is a function and expects at least one argument")
			}
			c.Dimensions = append(c.Dimensions, expr.Val)
		case *influxql.Call:
			// Ensure the call is time() and it has one or two duration arguments.
			// If we already have a duration
			if expr.Name != "time" {
				return nil, errors.New("only time() calls allowed in dimensions")
			} else if got := len(expr.Args); got < 1 || got > 2 {
				return nil, errors.New("time dimension expected 1 or 2 arguments")
			} else if lit, ok := expr.Args[0].(*influxql.DurationLiteral); !ok {
				return nil, errors.New("time dimension must have duration argument")
			} else if c.Interval.Duration != 0 {
				return nil, errors.New("multiple time dimensions not allowed")
			} else {
				c.Interval.Duration = lit.Val
				if len(expr.Args) == 2 {
					switch lit := expr.Args[1].(type) {
					case *influxql.DurationLiteral:
						c.Interval.Offset = lit.Val % c.Interval.Duration
					case *influxql.TimeLiteral:
						c.Interval.Offset = lit.Val.Sub(lit.Val.Truncate(c.Interval.Duration))
					case *influxql.Call:
						if lit.Name != "now" {
							return nil, errors.New("time dimension offset function must be now()")
						} else if len(lit.Args) != 0 {
							return nil, errors.New("time dimension offset now() function requires no arguments")
						}
						now := c.Options.Now
						c.Interval.Offset = now.Sub(now.Truncate(c.Interval.Duration))
					default:
						return nil, errors.New("time dimension offset must be duration or now()")
					}
				}
			}
		case *influxql.Wildcard:
			return nil, errors.New("unimplemented")
		case *influxql.RegexLiteral:
			return nil, errors.New("unimplemented")
		default:
			return nil, errors.New("only time and tag dimensions allowed")
		}
	}

	for _, f := range stmt.Fields {
		if ref, ok := f.Expr.(*influxql.VarRef); ok && ref.Val == "time" {
			continue
		}

		in, out := NewEdge(nil)
		field := &compiledField{Field: f, Output: out}
		if err := field.compileExpr(f.Expr, in); err != nil {
			return nil, err
		}
		c.Fields = append(c.Fields, field)
	}

	if err := c.validateFields(); err != nil {
		return nil, err
	}
	if err := c.linkAuxiliaryFields(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *compiledStatement) Select(plan *Plan) ([]*ReadEdge, error) {
	out := make([]*ReadEdge, 0, len(c.Fields))
	for _, f := range c.Fields {
		plan.AddTarget(f.Output)
		out = append(out, f.Output)
	}
	return out, nil
}

// requireAuxiliaryFields signals to the global state that we will need
// auxiliary fields to resolve some of the symbols. Instantiating it here lets
// us return an error if auxiliary fields are not compatible with some other
// part of the global state before we start contacting the shards for type
// information.
func (c *compiledStatement) requireAuxiliaryFields() {
	if c.AuxiliaryFields == nil {
		c.AuxiliaryFields = &AuxiliaryFields{}
	}
}
