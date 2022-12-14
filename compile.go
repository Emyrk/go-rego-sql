package go_rego

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Emyrk/go-rego/sqlast"
	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/rego"
)

type ConvertConfig struct {
	// VariableConverter is called each time a var is encountered. This creates
	// the SQL ast for the variable.
	VariableConverter sqlast.VariableMatcher
}

func NoACLConverter() *sqlast.VariableConverter {
	matcher := sqlast.NewVariableConverter().RegisterMatcher(
		// Basic strings
		sqlast.StringVarMatcher("organization_id :: text", []string{"input", "object", "org_owner"}),
		sqlast.StringVarMatcher("owner_id :: text", []string{"input", "object", "owner"}),
	)
	aclGroups := aclGroupMatchers(matcher)
	for i := range aclGroups {
		// Disable acl groups
		matcher.RegisterMatcher(aclGroups[i].Disable())
	}

	return matcher
}

func DefaultVariableConverter() *sqlast.VariableConverter {
	matcher := sqlast.NewVariableConverter().RegisterMatcher(
		// Basic strings
		sqlast.StringVarMatcher("organization_id :: text", []string{"input", "object", "org_owner"}),
		sqlast.StringVarMatcher("owner_id :: text", []string{"input", "object", "owner"}),
	)
	aclGroups := aclGroupMatchers(matcher)
	for i := range aclGroups {
		matcher.RegisterMatcher(aclGroups[i])
	}

	return matcher
}

func aclGroupMatchers(c *sqlast.VariableConverter) []ACLGroupVar {
	return []ACLGroupVar{
		ACLGroupMatcher(c, "group_acl", []string{"input", "object", "acl_group_list"}),
		ACLGroupMatcher(c, "user_acl", []string{"input", "object", "acl_user_list"}),
	}
}

func ConvertRegoAst(cfg ConvertConfig, partial *rego.PartialQueries) (sqlast.BooleanNode, error) {
	if len(partial.Queries) == 0 {
		// Always deny
		return sqlast.Bool(false), nil
	}

	for _, q := range partial.Queries {
		// An empty query in rego means "true"
		if len(q) == 0 {
			// Always allow
			return sqlast.Bool(true), nil
		}
	}

	var queries []sqlast.BooleanNode
	var builder strings.Builder
	for i, q := range partial.Queries {
		converted, err := convertQuery(cfg, q)
		if err != nil {
			return nil, fmt.Errorf("query %s: %w", q.String(), err)
		}

		boolConverted, ok := converted.(sqlast.BooleanNode)
		if !ok {
			return nil, fmt.Errorf("query %s: not a boolean expression", q.String())
		}

		if i != 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(q.String())
		queries = append(queries, boolConverted)
	}

	return sqlast.Or(sqlast.RegoSource(builder.String()), queries...), nil
}

func convertQuery(cfg ConvertConfig, q ast.Body) (sqlast.BooleanNode, error) {
	var expressions []sqlast.BooleanNode
	for _, e := range q {
		exp, err := convertExpression(cfg, e)
		if err != nil {
			return nil, fmt.Errorf("expression %s: %w", e.String(), err)
		}

		expressions = append(expressions, exp)
	}

	return sqlast.And(sqlast.RegoSource(q.String()), expressions...), nil
}

func convertExpression(cfg ConvertConfig, e *ast.Expr) (sqlast.BooleanNode, error) {
	if e.IsCall() {
		n, err := convertCall(cfg, e.Terms.([]*ast.Term))
		if err != nil {
			return nil, fmt.Errorf("call: %w", err)
		}

		boolN, ok := n.(sqlast.BooleanNode)
		if !ok {
			return nil, fmt.Errorf("call %q: not a boolean expression", e.String())
		}
		return boolN, nil
	}

	// If it's not a call, it is a single term
	if term, ok := e.Terms.(*ast.Term); ok {
		ty, err := convertTerm(cfg, term)
		if err != nil {
			return nil, fmt.Errorf("convert term %s: %w", term.String(), err)
		}

		tyBool, ok := ty.(sqlast.BooleanNode)
		if !ok {
			return nil, fmt.Errorf("convert term %s is not a boolean: %w", term.String(), err)
		}

		return tyBool, nil
	}

	return nil, fmt.Errorf("expression %s not supported", e.String())
}

// convertCall converts a function call to a SQL expression.
func convertCall(cfg ConvertConfig, call ast.Call) (sqlast.Node, error) {
	// Operator is the first term
	op := call[0]
	var args []*ast.Term
	if len(call) > 1 {
		args = call[1:]
	}

	opString := op.String()
	switch op.String() {
	case "neq", "eq", "equals", "equal":
		args, err := convertTerms(cfg, args, 2)
		if err != nil {
			return nil, fmt.Errorf("arguments: %w", err)
		}

		not := false
		if opString == "neq" || opString == "notequals" || opString == "notequal" {
			not = true
		}

		return sqlast.Equality(not, args[0], args[1]), nil
	case "internal.member_2":
		args, err := convertTerms(cfg, args, 2)
		if err != nil {
			return nil, fmt.Errorf("arguments: %w", err)
		}

		return sqlast.MemberOf(args[0], args[1]), nil
	default:
		return nil, fmt.Errorf("operator %s not supported", op)
	}
}

func convertTerms(cfg ConvertConfig, terms []*ast.Term, expected int) ([]sqlast.Node, error) {
	if len(terms) != expected {
		return nil, fmt.Errorf("expected %d terms, got %d", expected, len(terms))
	}

	result := make([]sqlast.Node, 0, len(terms))
	for _, t := range terms {
		term, err := convertTerm(cfg, t)
		if err != nil {
			return nil, fmt.Errorf("term: %w", err)
		}
		result = append(result, term)
	}

	return result, nil
}

func convertTerm(cfg ConvertConfig, term *ast.Term) (sqlast.Node, error) {
	source := sqlast.RegoSource(term.String())
	switch t := term.Value.(type) {
	case ast.Var:
		return nil, fmt.Errorf("var not yet supported")
	case ast.Ref:
		if len(t) == 0 {
			// A reference with no text is a variable with no name?
			// This makes no sense.
			return nil, fmt.Errorf("empty ref not supported")
		}

		if cfg.VariableConverter == nil {
			return nil, fmt.Errorf("no variable converter provided to handle variables")
		}

		// The structure of references is as follows:
		// 1. All variables start with a regoAst.Var as the first term.
		// 2. The next term is either a regoAst.String or a regoAst.Var.
		//	- regoAst.String if a static field name or index.
		//	- regoAst.Var if the field reference is a variable itself. Such as
		//    the wildcard "[_]"
		// 3. Repeat 1-2 until the end of the reference.
		node, ok := cfg.VariableConverter.ConvertVariable(t)
		if !ok {
			return nil, fmt.Errorf("variable %q cannot be converted", t.String())
		}
		return node, nil
	case ast.String:
		return sqlast.String(string(t)), nil
	case ast.Number:
		return sqlast.Number(source, json.Number(t)), nil
	case ast.Boolean:
		return sqlast.Bool(bool(t)), nil
	case *ast.Array:
		elems := make([]sqlast.Node, 0, t.Len())
		for i := 0; i < t.Len(); i++ {
			value, err := convertTerm(cfg, t.Elem(i))
			if err != nil {
				return nil, fmt.Errorf("array element %d in %q: %w", i, t.String(), err)
			}
			elems = append(elems, value)
		}
		return sqlast.Array(source, elems...)
	case ast.Object:
		return nil, fmt.Errorf("object not yet supported")
	case ast.Set:
		// Just treat a set like an array for now.
		arr := t.Sorted()
		return convertTerm(cfg, &ast.Term{
			Value:    arr,
			Location: term.Location,
		})
	case ast.Call:
		// This is a function call
		return convertCall(cfg, t)
	default:
		return nil, fmt.Errorf("%T not yet supported", t)
	}
}
