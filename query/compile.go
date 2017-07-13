package query

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/influxdata/influxdb/influxql"
)

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

	// FunctionCalls holds a reference to the output edge of all of the
	// function calls that have been instantiated.
	FunctionCalls []*ReadEdge

	// OnlySelectors is set to true when there are no aggregate functions.
	OnlySelectors bool

	// TopBottomFunction is set to top or bottom when one of those functions are
	// used in the statement.
	TopBottomFunction string

	// AuxFields holds a mapping to the auxiliary fields that need to be
	// selected. This maps the raw VarRef to a pointer to a shared VarRef. The
	// pointer is used for instantiating references to the shared variable so
	// type mapping gets shared.
	AuxiliaryFields *AuxiliaryFields

	// OutputEdges holds the outermost edges that will be used to read from
	// when returning results.
	OutputEdges []*ReadEdge

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
		OutputEdges:   make([]*ReadEdge, 0, len(stmt.Fields)),
		Options:       opt,
	}
}

func (c *compiledStatement) compileExpr(expr influxql.Expr) (*ReadEdge, error) {
	switch expr := expr.(type) {
	case *influxql.VarRef:
		// If there is no instance of AuxiliaryFields, instantiate one now.
		if c.AuxiliaryFields == nil {
			c.AuxiliaryFields = &AuxiliaryFields{}
		}
		return c.AuxiliaryFields.Iterator(expr), nil
	case *influxql.Call:
		switch expr.Name {
		case "count", "min", "max", "sum", "first", "last", "mean":
			return c.compileFunction(expr)
		case "distinct":
			return c.compileDistinct(expr)
		case "top", "bottom":
			return c.compileTopBottom(expr)
		default:
			return nil, errors.New("unimplemented")
		}
	case *influxql.Distinct:
		return c.compileDistinct(expr.NewCall())
	case *influxql.BinaryExpr:
		// Check if either side is a literal so we only compile one side if it is.
		if _, ok := expr.LHS.(influxql.Literal); ok {
		} else if _, ok := expr.RHS.(influxql.Literal); ok {
		} else {
			lhs, err := c.compileExpr(expr.LHS)
			if err != nil {
				return nil, err
			}
			rhs, err := c.compileExpr(expr.RHS)
			if err != nil {
				return nil, err
			}
			node := &BinaryExpr{LHS: lhs, RHS: rhs, Op: expr.Op}
			lhs.Node, rhs.Node = node, node

			var out *ReadEdge
			node.Output, out = NewEdge(node)
			return out, nil
		}
	}
	return nil, errors.New("unimplemented")
}

func (c *compiledStatement) compileFunction(expr *influxql.Call) (*ReadEdge, error) {
	if exp, got := 1, len(expr.Args); exp != got {
		return nil, fmt.Errorf("invalid number of arguments for %s, expected %d, got %d", expr.Name, exp, got)
	}

	// Generate the source of the function.
	var input *ReadEdge
	if expr.Name == "count" {
		// If we have count(), the argument may be a distinct() call.
		if arg0, ok := expr.Args[0].(*influxql.Call); ok && arg0.Name == "distinct" {
			d, err := c.compileDistinct(arg0)
			if err != nil {
				return nil, err
			}
			input = d
		} else if arg0, ok := expr.Args[0].(*influxql.Distinct); ok {
			d, err := c.compileDistinct(arg0.NewCall())
			if err != nil {
				return nil, err
			}
			input = d
		}
	}

	// Must be a variable reference.
	if input == nil {
		arg0, ok := expr.Args[0].(*influxql.VarRef)
		if !ok {
			return nil, fmt.Errorf("expected field argument in %s()", expr.Name)
		}
		input = c.compileVarRef(arg0, nil)
	}

	// Retrieve the variable reference.
	call := &FunctionCall{Name: expr.Name}
	call.Input, input.Node = input, call

	// Mark down some meta properties related to the function for query validation.
	switch expr.Name {
	case "top", "bottom":
		if c.TopBottomFunction == "" {
			c.TopBottomFunction = expr.Name
		}
	case "max", "min", "first", "last", "percentile", "sample":
	default:
		c.OnlySelectors = false
	}

	var out *ReadEdge
	call.Output, out = NewEdge(call)
	c.FunctionCalls = append(c.FunctionCalls, out)
	return out, nil
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
			c.AuxiliaryFields.Input = c.compileVarRef(nil, c.AuxiliaryFields)
		}
	}
	return nil
}

func (c *compiledStatement) compileDistinct(call *influxql.Call) (*ReadEdge, error) {
	if len(call.Args) == 0 {
		return nil, errors.New("distinct function requires at least one argument")
	} else if len(call.Args) != 1 {
		return nil, errors.New("distinct function can only have one argument")
	}

	arg0, ok := call.Args[0].(*influxql.VarRef)
	if !ok {
		return nil, errors.New("expected field argument in distinct()")
	}

	d := &Distinct{}
	d.Input = c.compileVarRef(arg0, d)

	var out *ReadEdge
	d.Output, out = NewEdge(d)
	c.FunctionCalls = append(c.FunctionCalls, out)
	return out, nil
}

func (c *compiledStatement) compileTopBottom(call *influxql.Call) (*ReadEdge, error) {
	if c.TopBottomFunction != "" {
		return nil, fmt.Errorf("selector function %s() cannot be combined with other functions", c.TopBottomFunction)
	}

	if exp, got := 2, len(call.Args); got < exp {
		return nil, fmt.Errorf("invalid number of arguments for %s, expected at least %d, got %d", call.Name, exp, got)
	}

	ref, ok := call.Args[0].(*influxql.VarRef)
	if !ok {
		return nil, fmt.Errorf("expected field argument in %s()", call.Name)
	}

	var dimensions []influxql.VarRef
	if len(call.Args) > 2 {
		dimensions = make([]influxql.VarRef, 0, len(call.Args))
		for _, v := range call.Args[1 : len(call.Args)-1] {
			if ref, ok := v.(*influxql.VarRef); ok {
				dimensions = append(dimensions, *ref)
			} else {
				return nil, fmt.Errorf("only fields or tags are allowed in %s(), found %s", call.Name, v)
			}
		}
	}

	limit, ok := call.Args[len(call.Args)-1].(*influxql.IntegerLiteral)
	if !ok {
		return nil, fmt.Errorf("expected integer as last argument in %s(), found %s", call.Name, call.Args[len(call.Args)-1])
	} else if limit.Val <= 0 {
		return nil, fmt.Errorf("limit (%d) in %s function must be at least 1", limit.Val, call.Name)
	}
	c.TopBottomFunction = call.Name

	selector := &TopBottomSelector{Dimensions: dimensions}
	selector.Input = c.compileVarRef(ref, selector)

	var out *ReadEdge
	selector.Output, out = NewEdge(selector)
	c.FunctionCalls = append(c.FunctionCalls, out)
	return out, nil
}

func (c *compiledStatement) compileVarRef(ref *influxql.VarRef, node Node) *ReadEdge {
	merge := &Merge{}
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
			panic("unimplemented")
		}
	}

	var out *ReadEdge
	merge.Output, out = AddEdge(merge, node)
	return out
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

		out, err := c.compileExpr(f.Expr)
		if err != nil {
			return nil, err
		}
		c.OutputEdges = append(c.OutputEdges, out)
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
	for _, out := range c.OutputEdges {
		plan.AddTarget(out)
	}
	return c.OutputEdges, nil
}
