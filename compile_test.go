package go_rego

import (
	"context"
	"github.com/open-policy-agent/opa/rego"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestPartialQueries(t *testing.T) {
	cfg := CompileConfig{
		VariableTypes: NewTree().
			AddElement(strings.Split("input.post.deleted", "."), Boolean{}, StaticName("deleted")).
			AddElement(strings.Split("input.post.author", "."), String{}, StaticName("author")).
			AddElement(strings.Split("input.post.can", "."), String{}, StaticName("can")).
			AddElement(strings.Split("input.post.authors", "."), Map{ValueType: String{}},
				RegexColumnNameReplace(`input\.post\.authors\.(.*)`, "authors->$1")).
			AddElement(strings.Split("input.post.posts", "."), Array{elemType: String{}}, StaticName("posts")).
			AddElement(strings.Split("input.post.can_list", "."), Array{elemType: String{}}, StaticName("can_list")).
			AddElement(strings.Split("input.post.list", "."), Array{elemType: String{}}, StaticName("list")).
			AddElement(strings.Split("input.post.moderators", "."), Array{elemType: String{}}, StaticName("moderators")),
	}
	//opts := ast.ParserOptions{AllFutureKeywords: true}
	testCases := []struct {
		Name        string
		Input       map[string]interface{}
		Unknowns    []string
		Rego        string
		ExpectedSQL string
		ExpectError bool
	}{
		{
			Name: "AlwaysFalse",
			Rego: `
 			package example
			allow = true {
   	 			input.method = "GET"
    			input.path = ["posts"]
			}`,
			Input: map[string]interface{}{
				"method": "GET",
				"path":   []string{"users"},
				"user":   "bob",
			},
			ExpectedSQL: "false",
			Unknowns:    []string{"none"},
		},
		{
			Name: "AlwaysTrue",
			Rego: `
 			package example
			allow = true {
   	 			input.method = "GET"
    			input.path = ["posts"]
			}`,
			Input: map[string]interface{}{
				"method": "GET",
				"path":   []string{"posts"},
				"user":   "bob",
			},
			ExpectedSQL: "true",
			Unknowns:    []string{"none"},
		},
		{
			Name: "SingleObject",
			// "bob" = input.post.author
			Rego: `
			package example
			allow {
				input.post.author = input.user
			}
			`,
			Input: map[string]interface{}{
				"user": "bob",
			},
			ExpectedSQL: "'bob' = author",
			Unknowns:    []string{"input.post.author"},
		},
		{
			Name: "RefBoolean",
			// input.post.deleted
			Rego: `
			package example
			allow {
				input.post.deleted
			}
			`,
			Input:       map[string]interface{}{},
			ExpectedSQL: "deleted",
			Unknowns:    []string{"input.post.deleted"},
		},
		{
			Name: "RefWithNumber",
			// Query 0: "bob" = input.post.author.name; "bob" = input.post.list[0]
			Rego: `
			package example
			allow {
				input.post.authors["name"] = input.user
				input.post.list[0] = input.user
			}
			`,
			Input: map[string]interface{}{
				"user": "bob",
			},
			// TODO: Convert vars to columns
			ExpectedSQL: "input.post.author = 'bob",
			Unknowns:    []string{"input.post.author", "input.post.list"},
		},
		{
			Name: "Array",
			// Query 0: "bob" = input.post.author
			// Query 1: "bob" = input.post.moderators[_]
			Rego: `
			package example
			allow {
				can_edit
			}

			can_edit {
				input.post.author = input.user
			}
			can_edit {
				input.post.moderators[_] = input.user
			}		

			`,
			Input: map[string]interface{}{
				"user": "bob",
			},
			// TODO: Convert vars to columns
			ExpectedSQL: "input.post.author = 'bob' OR 'bob' = ANY(input.post.moderators)",
			Unknowns:    []string{"input.post.author", "input.post.moderators"},
		},
		{
			Name: "ArrayIntersection",
			// Query 0: internal.member_2(input.can_list[_], ["edit", "*"])
			// Query 1: internal.member_2(input.can, ["edit", "*"])
			Rego: `
			package example
			import future.keywords.in
			allow {
				input.can in ["edit", "*"]
			}

			allow {
				input.can_list[_] in ["edit", "*"]			
			}
			`,
			Input: map[string]interface{}{},
			// TODO: Convert vars to columns
			ExpectedSQL: "input.can_list && ARRAY['edit', '*'] OR input.can = ANY(ARRAY ['edit', '*'])",
			Unknowns:    []string{"input.can_list", "input.can"},
		},
		{
			Name: "EveryTerm",
			// "bob" = input.posts[_].author; input.posts[_]
			Rego: `
			package example
			allow = true {
				input.method = "GET"
				input.path = ["posts"]
				allowed[x]
			}
			
			allowed[x] {
				x := input.posts[_]
				x.author == input.user
			}	
			`,
			Input: map[string]interface{}{
				"method": "GET",
				"path":   []string{"posts"},
				"user":   "bob",
			},
			ExpectedSQL: "true",
			Unknowns:    []string{"input.posts"},
		},

		// Failures
		{
			Name: "RefString",
			Rego: `
			package example
			allow {
				input.post.author
			}
			`,
			Input:       map[string]interface{}{},
			Unknowns:    []string{"input.post.author"},
			ExpectError: true,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			part, err := rego.New(
				rego.Query("data.example.allow == true"),
				rego.Module("policy.rego", tc.Rego),
				rego.Input(tc.Input),
				rego.Unknowns(tc.Unknowns),
			).Partial(ctx)
			require.NoError(t, err)

			for i, q := range part.Queries {
				t.Logf("Query %d: %s", i, q.String())
			}
			for i, s := range part.Support {
				t.Logf("Support %d: %s", i, s.String())
			}

			sql, err := CompileSQL(cfg, part)
			if tc.ExpectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err, "compile")
				require.Equal(t, tc.ExpectedSQL, sql, "sql match")
			}
		})
	}
}