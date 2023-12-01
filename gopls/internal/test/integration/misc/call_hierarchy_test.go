// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
package misc

import (
	"fmt"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/tools/gopls/internal/lsp/protocol"
	. "golang.org/x/tools/gopls/internal/test/integration"
)

// Test for golang/go#49125
func TestCallHierarchy_Issue49125(t *testing.T) {
	const files = `
	-- go.mod --
module mod.com

go 1.12
-- p.go --
package pkg
`
	// TODO(rfindley): this could probably just be a marker test.
	Run(t, files, func(t *testing.T, env *Env) {
		env.OpenFile("p.go")
		loc := env.RegexpSearch("p.go", "pkg")

		var params protocol.CallHierarchyPrepareParams
		params.TextDocument.URI = loc.URI
		params.Position = loc.Range.Start

		// Check that this doesn't panic.
		env.Editor.Server.PrepareCallHierarchy(env.Ctx, &params)
	})
}

// TODO: link issue.
func TestCallHierarchy_IncomingCallsWithAnonymousFunctions(t *testing.T) {
	const files = `
	-- go.mod --
module mod.com

go 1.18
-- p.go --
package pkg

import "sync"

var once sync.Once

func A() {
	once.Do(func() {
		B()
		B()

		B1()
	})

	B()
}

func B() { C() }

func C() { D() }

var B1 = func() {
	D()
}

func D() {}
`
	Run(t, files, func(t *testing.T, env *Env) {
		env.OpenFile("p.go")
		loc := env.RegexpSearch("p.go", "func (D)")

		type X struct {
			Name           string
			Range          string
			SelectionRange string
			FromRanges     string
			Next           []X
		}

		var walk func(protocol.CallHierarchyItem, []protocol.Range) X
		walk = func(root protocol.CallHierarchyItem, fromRanges []protocol.Range) X {
			res := X{
				Name:           root.Name,
				Range:          fmt.Sprint(root.Range),
				SelectionRange: fmt.Sprint(root.SelectionRange),
			}
			if len(fromRanges) > 0 {
				res.FromRanges = fmt.Sprint(fromRanges)
			}

			var params protocol.CallHierarchyIncomingCallsParams
			params.Item.URI = loc.URI
			params.Item.Range.Start = root.Range.Start
			calls, err := env.Editor.Server.IncomingCalls(env.Ctx, &params)
			if err != nil {
				t.Fatalf("IncomingCalls failed: %s", err)
			}

			for _, c := range calls {
				res.Next = append(res.Next, walk(c.From, c.FromRanges))
			}
			sort.Slice(res.Next, func(i, j int) bool {
				a, b := res.Next[i], res.Next[j]
				if a.Range != b.Range {
					return a.Range < b.Range
				}
				return a.Name < b.Name
			})
			return res
		}

		var prepareParams protocol.CallHierarchyPrepareParams
		prepareParams.TextDocument.URI = loc.URI
		prepareParams.Position = loc.Range.Start

		// Check that this doesn't panic.
		roots, err := env.Editor.Server.PrepareCallHierarchy(env.Ctx, &prepareParams)
		if err != nil {
			t.Fatalf("PrepareCallHierarchy failed: %s", err)
		}
		if l := len(roots); l != 1 {
			t.Errorf("PrepareCallHierarchy returned %d items, want exactly 1", l)
		}

		got := walk(roots[0], nil)

		want := X{
			Name:           "D",
			Range:          "25:5-25:6",
			SelectionRange: "25:5-25:6",
			Next: []X{
				{
					Name:           "C",
					Range:          "19:5-19:6", // func name 'C'
					SelectionRange: "19:5-19:6", // func name 'C'
					FromRanges:     "[19:11-19:12]",
					Next: []X{
						{
							Name:           "B",
							Range:          "17:5-17:6", // func name 'B'
							SelectionRange: "17:5-17:6", // func name 'B'
							FromRanges:     "[17:11-17:12]",
							Next: []X{
								{
									Name:           "A",
									Range:          "6:5-6:6", // func name 'A'
									SelectionRange: "6:5-6:6", // func name 'A'
									FromRanges:     "[14:1-14:2]",
								},
								{
									Name:           "A.func()",
									Range:          "6:5-6:6",  // func name 'A'
									SelectionRange: "7:9-7:13", // func literal 'Do(func())'
									FromRanges:     "[8:2-8:3 9:2-9:3]",
								},
							},
						},
					},
				},
				{
					Name:           "B1.func()",
					Range:          "21:4-21:6",  // variable name 'B1'
					SelectionRange: "21:9-21:13", // func literal '= func()'
					FromRanges:     "[22:1-22:2]",
					Next: []X{
						{
							Name:           "A.func()",
							Range:          "6:5-6:6",  // func name 'A'
							SelectionRange: "7:9-7:13", // func literal 'Do(func()'
							FromRanges:     "[11:2-11:4]",
						},
					},
				},
			},
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("mismatching IncomingCalls result (-want +got):\n%s", diff)
		}
	})
}
