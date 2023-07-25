package transform

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	astro "github.com/withastro/compiler/internal"
	"github.com/withastro/compiler/internal/handler"
	"github.com/withastro/compiler/internal/js_scanner"
	"github.com/withastro/compiler/internal/loc"
	a "golang.org/x/net/html/atom"
)

const TRANSITION_ANIMATE = "transition:animate"
const TRANSITION_NAME = "transition:name"

type TransformOptions struct {
	Scope               string
	Filename            string
	NormalizedFilename  string
	InternalURL         string
	SourceMap           string
	AstroGlobalArgs     string
	ScopedStyleStrategy string
	Compact             bool
	ResultScopedSlot    bool
	ResolvePath         func(string) string
	PreprocessStyle     interface{}
	ExtraHoist          []string
}

func Transform(doc *astro.Node, opts TransformOptions, h *handler.Handler) *astro.Node {
	shouldScope := len(doc.Styles) > 0 && ScopeStyle(doc.Styles, opts)
	definedVars := GetDefineVars(doc.Styles)
	didAddDefinedVars := false
	i := 0
	walk(doc, func(n *astro.Node) {
		i++
		ExtractScript(doc, n, &opts, h)
		AddComponentProps(doc, n, &opts)
		if shouldScope {
			ScopeElement(n, opts)
		}
		if opts.ExperimentalTransitions && (HasAttr(n, TRANSITION_ANIMATE) || HasAttr(n, TRANSITION_NAME)) {
			doc.Transition = true
			n.TransitionScope = astro.HashString(fmt.Sprintf("%s-%v", opts.Scope, i))
		}
		if len(definedVars) > 0 {
			didAdd := AddDefineVars(n, definedVars)
			if !didAddDefinedVars {
				didAddDefinedVars = didAdd
			}
		}
		mergeClassList(doc, n, &opts)
		if n.DataAtom == a.Head && !IsImplicitNode(n) {
			doc.ContainsHead = true
		}
	})
	if len(definedVars) > 0 && !didAddDefinedVars {
		for _, style := range doc.Styles {
			for _, a := range style.Attr {
				if a.Key == "define:vars" {
					h.AppendWarning(&loc.ErrorWithRange{
						Code:  loc.WARNING_CANNOT_DEFINE_VARS,
						Text:  "Unable to inject `define:vars` declaration",
						Range: loc.Range{Loc: a.KeyLoc, Len: len("define:vars")},
						Hint:  "Try wrapping this component in an element so that Astro can inject a \"style\" attribute.",
					})
				}
			}
		}
	}
	NormalizeSetDirectives(doc, h)

	// Important! Remove scripts from original location *after* walking the doc
	for _, script := range doc.Scripts {
		script.Parent.RemoveChild(script)
	}

	// If we've emptied out all the nodes, this was a Fragment that only contained hoisted elements
	// Add an empty FrontmatterNode to allow the empty component to be printed
	if doc.FirstChild == nil {
		empty := &astro.Node{
			Type: astro.FrontmatterNode,
		}
		empty.AppendChild(&astro.Node{
			Type: astro.TextNode,
			Data: "",
		})
		doc.AppendChild(empty)
	}

	TrimTrailingSpace(doc)

	if opts.Compact {
		collapseWhitespace(doc)
	}

	return doc
}

func ExtractStyles(doc *astro.Node) {
	walk(doc, func(n *astro.Node) {
		if n.Type == astro.ElementNode && n.DataAtom == a.Style {
			if HasSetDirective(n) || HasInlineDirective(n) {
				return
			}
			// Ignore styles in svg/noscript/etc
			if !IsHoistable(n) {
				return
			}
			// prepend node to maintain authored order
			doc.Styles = append([]*astro.Node{n}, doc.Styles...)
		}
	})
	// Important! Remove styles from original location *after* walking the doc
	for _, style := range doc.Styles {
		style.Parent.RemoveChild(style)
	}
}

func NormalizeSetDirectives(doc *astro.Node, h *handler.Handler) {
	var nodes []*astro.Node
	var directives []*astro.Attribute
	walk(doc, func(n *astro.Node) {
		if n.Type == astro.ElementNode && HasSetDirective(n) {
			for _, attr := range n.Attr {
				if attr.Key == "set:html" || attr.Key == "set:text" {
					nodes = append(nodes, n)
					directives = append(directives, &attr)
					return
				}
			}
		}
	})

	if len(nodes) > 0 {
		for i, n := range nodes {
			directive := directives[i]
			n.RemoveAttribute(directive.Key)

			var nodeToAppend *astro.Node
			var shouldWrapInQuotes,
				isTemplateLiteralAttribute,
				isQuotedAttribute,
				isExpressionAttribute,
				shouldWrapInTemplateLiteral,
				shouldAddExpression bool

			switch directive.Type {
			case astro.QuotedAttribute:
				isQuotedAttribute = true
			case astro.TemplateLiteralAttribute:
				isTemplateLiteralAttribute = true
			case astro.ExpressionAttribute:
				isExpressionAttribute = true
			}

			if directive.Key == "set:html" && isQuotedAttribute {
				shouldWrapInQuotes = true
			}
			if isTemplateLiteralAttribute {
				shouldWrapInTemplateLiteral = true
			}
			if directive.Key == "set:html" || (directive.Key == "set:text" && isTemplateLiteralAttribute) || isExpressionAttribute {
				shouldAddExpression = true
			}

			l := make([]loc.Loc, 1)
			l = append(l, directive.ValLoc)
			data := directive.Val

			if shouldWrapInQuotes {
				data = fmt.Sprintf("\"%s\"", data)
			}

			if shouldWrapInTemplateLiteral {
				data = fmt.Sprintf("`%s`", data)
			}

			if directive.Key == "set:html" && isExpressionAttribute {
				data = fmt.Sprintf("$$unescapeHTML(%s)", data)
			}
			if shouldAddExpression {
				nodeToAppend = &astro.Node{
					Type:       astro.ElementNode,
					Data:       "astro:expression",
					Expression: true,
				}
				nodeToAppend.AppendChild(&astro.Node{
					Type: astro.TextNode,
					Data: data,
					Loc:  l,
				})
			} else {
				nodeToAppend = &astro.Node{
					Type: astro.TextNode,
					Data: data,
					Loc:  l,
				}
			}

			shouldWarn := false
			// Remove all existing children
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if !shouldWarn {
					shouldWarn = c.Type == astro.CommentNode || (c.Type == astro.TextNode && len(strings.TrimSpace(c.Data)) != 0)
				}
				n.RemoveChild(c)
			}
			if shouldWarn {
				h.AppendWarning(&loc.ErrorWithRange{
					Code:  loc.WARNING_SET_WITH_CHILDREN,
					Text:  fmt.Sprintf("%s directive will overwrite child nodes.", directive.Key),
					Range: loc.Range{Loc: directive.KeyLoc, Len: len(directive.Key)},
					Hint:  "Remove the child nodes to suppress this warning.",
				})
			}
			n.AppendChild(nodeToAppend)
		}
	}
}

func TrimTrailingSpace(doc *astro.Node) {
	if doc.LastChild == nil {
		return
	}

	if doc.LastChild.Type == astro.TextNode {
		doc.LastChild.Data = strings.TrimRightFunc(doc.LastChild.Data, unicode.IsSpace)
		return
	}

	n := doc.LastChild
	for i := 0; i < 2; i++ {
		// Loop through implicit nodes to find final trailing text node (html > body > #text)
		if n != nil && n.Type == astro.ElementNode && IsImplicitNode(n) {
			n = n.LastChild
			continue
		} else {
			n = nil
			break
		}
	}

	// Collapse all trailing text nodes
	for n != nil && n.Type == astro.TextNode {
		n.Data = strings.TrimRightFunc(n.Data, unicode.IsSpace)
		n = n.PrevSibling
	}
}

func isRawElement(n *astro.Node) bool {
	if n.Type == astro.FrontmatterNode {
		return true
	}
	rawTags := []string{"pre", "listing", "iframe", "noembed", "noframes", "math", "plaintext", "script", "style", "textarea", "title", "xmp"}
	for _, tag := range rawTags {
		if n.Data == tag {
			return true
		}
		for _, attr := range n.Attr {
			if attr.Key == "is:raw" {
				return true
			}
		}
	}
	return false
}

func collapseWhitespace(doc *astro.Node) {
	walk(doc, func(n *astro.Node) {
		if n.Type == astro.TextNode {
			// Don't trim any whitespace if the node or any of its ancestors is raw
			if n.Closest(isRawElement) != nil {
				return
			}

			// Trim the whitespace on each end of top-level expressions
			if n.Parent != nil && n.Parent.Expression {
				// Trim left whitespace in the first child
				if n.PrevSibling == nil {
					n.Data = strings.TrimLeftFunc(n.Data, unicode.IsSpace)
				}
				// Trim right whitespace in the last child
				if n.NextSibling == nil {
					n.Data = strings.TrimRightFunc(n.Data, unicode.IsSpace)
				}
				// Don't trim any more!
				return
			}

			// If the node is only whitespace, clear it
			if len(strings.TrimFunc(n.Data, unicode.IsSpace)) == 0 {
				n.Data = ""
				return
			}

			// Collapse left whitespace into a single space
			originalLen := len(n.Data)
			hasNewline := false
			n.Data = strings.TrimLeftFunc(n.Data, func(r rune) bool {
				if r == '\n' {
					hasNewline = true
				}
				return unicode.IsSpace(r)
			})
			if originalLen != len(n.Data) {
				if hasNewline {
					n.Data = "\n" + n.Data
				} else {
					n.Data = " " + n.Data
				}
			}
			// Collapse right whitespace into a single space
			originalLen = len(n.Data)
			hasNewline = false
			n.Data = strings.TrimRightFunc(n.Data, func(r rune) bool {
				if r == '\n' {
					hasNewline = true
				}
				return unicode.IsSpace(r)
			})
			if originalLen != len(n.Data) {
				if hasNewline {
					n.Data = n.Data + "\n"
				} else {
					n.Data = n.Data + " "
				}
			}
		}
	})
}

func ExtractScript(doc *astro.Node, n *astro.Node, opts *TransformOptions, h *handler.Handler) {
	if n.Type == astro.ElementNode && n.DataAtom == a.Script {
		if HasSetDirective(n) || HasInlineDirective(n) {
			return
		}
		// Ignore scripts in svg/noscript/etc
		if !IsHoistable(n) {
			return
		}

		// if <script>, hoist to the document root
		// If also using define:vars, that overrides the hoist tag.
		if (hasTruthyAttr(n, "hoist")) ||
			len(n.Attr) == 0 || (len(n.Attr) == 1 && n.Attr[0].Key == "src") || (len(n.Attr) == 1 && n.Attr[0].Key == "define:serverfunctions") {
			shouldAdd := true
			for _, attr := range n.Attr {
				if attr.Key == "hoist" {
					h.AppendWarning(&loc.ErrorWithRange{
						Code:  loc.WARNING_DEPRECATED_DIRECTIVE,
						Text:  "<script hoist> is no longer needed. You may remove the `hoist` attribute.",
						Range: loc.Range{Loc: n.Loc[0], Len: len(n.Data)},
					})
				}
				if attr.Key == "src" {
					if attr.Type == astro.ExpressionAttribute {
						shouldAdd = false
						h.AppendWarning(&loc.ErrorWithRange{
							Code:  loc.WARNING_UNSUPPORTED_EXPRESSION,
							Text:  "<script> uses an expression for the src attribute and will be ignored.",
							Hint:  fmt.Sprintf("Replace src={%s} with a string literal", attr.Val),
							Range: loc.Range{Loc: n.Loc[0], Len: len(n.Data)},
						})
						break
					}
				}
			}

			// prepend node to maintain authored order
			if shouldAdd {
				doc.Scripts = append([]*astro.Node{n}, doc.Scripts...)
			}
		} else {
			for _, attr := range n.Attr {
				if strings.HasPrefix(attr.Key, "client:") {
					h.AppendWarning(&loc.ErrorWithRange{
						Code:  loc.WARNING_IGNORED_DIRECTIVE,
						Text:  fmt.Sprintf("<script> does not need the %s directive and is always added as a module script.", attr.Key),
						Range: loc.Range{Loc: n.Loc[0], Len: len(n.Data)},
					})
				}
			}
		}
	}
}

func AddComponentProps(doc *astro.Node, n *astro.Node, opts *TransformOptions) {
	if n.Type == astro.ElementNode && (n.Component || n.CustomElement) {
		for _, attr := range n.Attr {
			if strings.HasPrefix(attr.Key, "client:") {
				parts := strings.Split(attr.Key, ":")
				directive := parts[1]

				// Add the hydration directive so it can be extracted statically.
				doc.HydrationDirectives[directive] = true

				hydrationAttr := astro.Attribute{
					Key: "client:component-hydration",
					Val: directive,
				}
				n.Attr = append(n.Attr, hydrationAttr)

				if attr.Key == "client:only" {
					doc.ClientOnlyComponentNodes = append([]*astro.Node{n}, doc.ClientOnlyComponentNodes...)

					match := matchNodeToImportStatement(doc, n)
					if match != nil {
						doc.ClientOnlyComponents = append(doc.ClientOnlyComponents, &astro.HydratedComponentMetadata{
							ExportName:   match.ExportName,
							Specifier:    match.Specifier,
							ResolvedPath: ResolveIdForMatch(match.Specifier, opts),
						})
					}

					break
				}
				// prepend node to maintain authored order
				doc.HydratedComponentNodes = append([]*astro.Node{n}, doc.HydratedComponentNodes...)

				match := matchNodeToImportStatement(doc, n)
				if match != nil {
					doc.HydratedComponents = append(doc.HydratedComponents, &astro.HydratedComponentMetadata{
						ExportName:   match.ExportName,
						Specifier:    match.Specifier,
						ResolvedPath: ResolveIdForMatch(match.Specifier, opts),
					})

					pathAttr := astro.Attribute{
						Key:  "client:component-path",
						Val:  fmt.Sprintf(`"%s"`, ResolveIdForMatch(match.Specifier, opts)),
						Type: astro.ExpressionAttribute,
					}
					n.Attr = append(n.Attr, pathAttr)

					exportAttr := astro.Attribute{
						Key:  "client:component-export",
						Val:  fmt.Sprintf(`"%s"`, match.ExportName),
						Type: astro.ExpressionAttribute,
					}
					n.Attr = append(n.Attr, exportAttr)
				}

				break
			}
		}
	}
}

type ImportMatch struct {
	ExportName string
	Specifier  string
}

func matchNodeToImportStatement(doc *astro.Node, n *astro.Node) *ImportMatch {
	var match *ImportMatch

	eachImportStatement(doc, func(stmt js_scanner.ImportStatement) bool {
		for _, imported := range stmt.Imports {

			if strings.Contains(n.Data, ".") && strings.HasPrefix(n.Data, fmt.Sprintf("%s.", imported.LocalName)) {
				exportName := n.Data
				if imported.ExportName == "*" {
					exportName = strings.Replace(exportName, fmt.Sprintf("%s.", imported.LocalName), "", 1)
				}
				match = &ImportMatch{
					ExportName: exportName,
					Specifier:  stmt.Specifier,
				}
				return false
			} else if imported.LocalName == n.Data {
				match = &ImportMatch{
					ExportName: imported.ExportName,
					Specifier:  stmt.Specifier,
				}
				return false
			}
		}

		return true
	})
	return match
}

func ResolveIdForMatch(id string, opts *TransformOptions) string {
	// Try custom resolvePath if provided
	if opts.ResolvePath != nil {
		return opts.ResolvePath(id)
	} else if opts.Filename != "<stdin>" && id[0] == '.' {
		return filepath.Join(filepath.Dir(opts.Filename), id)
	} else {
		return id
	}
}

func eachImportStatement(doc *astro.Node, cb func(stmt js_scanner.ImportStatement) bool) {
	if doc.FirstChild.Type == astro.FrontmatterNode && doc.FirstChild.FirstChild != nil {
		source := []byte(doc.FirstChild.FirstChild.Data)
		loc, statement := js_scanner.NextImportStatement(source, 0)
		for loc != -1 {
			if !cb(statement) {
				break
			}
			loc, statement = js_scanner.NextImportStatement(source, loc)
		}
	}
}

func walk(doc *astro.Node, cb func(*astro.Node)) {
	var f func(*astro.Node)
	f = func(n *astro.Node) {
		cb(n)
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(doc)
}

// This function merges the values of `class=""` and `class:list=""` in `class:list`
func mergeClassList(doc *astro.Node, n *astro.Node, opts *TransformOptions) {
	var classListAttrValue string
	var classListAttrIndex int = -1
	var classAttrValue string
	var classAttrIndex int = -1
	for i, a := range n.Attr {
		if a.Key == "class:list" {
			classListAttrValue = a.Val
			classListAttrIndex = i
		}
		if a.Key == "class" {
			classAttrValue = a.Val
			classAttrIndex = i
		}
	}
	if classListAttrIndex >= 0 && classAttrIndex >= 0 {
		// we append the prepend the value of class to class:list
		n.Attr[classListAttrIndex].Val = fmt.Sprintf("['%s', %s]", classAttrValue, classListAttrValue)
		// Now that the value of `class` is carried by `class:list`, we can remove the `class` node from the AST.
		// Doing so will allow us to generate valid HTML at runtime
		n.Attr = remove(n.Attr, classAttrIndex)
	}
}

func remove(slice []astro.Attribute, s int) []astro.Attribute {
	return append(slice[:s], slice[s+1:]...)
}
