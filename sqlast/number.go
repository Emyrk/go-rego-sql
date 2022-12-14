package sqlast

import (
	"encoding/json"
	"fmt"
)

type AstNumber struct {
	Source RegoSource
	// Value is intentionally vague as to if it's an integer or a float.
	// This defers that decision to the user. Rego keeps all numbers in this
	// type. If we were to source the type from something other than Rego,
	// we might want to make a Float and Int type which keep the original
	// precision.
	Value json.Number
}

func Number(source RegoSource, v json.Number) Node {
	return AstNumber{Value: v, Source: source}
}

func (AstNumber) UseAs() Node { return AstNumber{} }

func (n AstNumber) SQLString(cfg *SQLGenerator) string {
	// TODO: Verify that this is a valid AstNumber in sql
	return n.Value.String()
}

func (n AstNumber) EqualsSQLString(cfg *SQLGenerator, not bool, other Node) (string, error) {
	switch other.UseAs().(type) {
	case AstNumber:
		return basicSQLEquality(cfg, not, n, other), nil
	}

	return "", fmt.Errorf("unsupported equality: %T %s %T", n, equalsOp(not), other)
}
