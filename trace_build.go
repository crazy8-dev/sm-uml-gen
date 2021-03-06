package main

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"
)

func (p *ExecTrace) addTransition(su *StateUpdate) {
	mt := MethodTransition{}
	if conds := p.nearestCond(); conds != nil {
		mt.Condition = conds.buildCondition()
	}
	if p.migration != nil {
		mt.Migration = p.getInlineFuncExpr(p.migration, Execution)
	} else {
		mt.InheritMigration = true
	}

	switch p.md.MType {
	case DeclarationInit, Construction:
		mt.Transition = su.fullName()
	case 0:
		fallthrough
	default:
		if !p.addContextOpTransition(su, &mt) {
			if mt.Transition == "" {
				return
			}
			mt.Transition = "<unknown>"
		}
	}
	p.md.AddTransition(mt)
}

func (p *ExecTrace) addContextOpTransition(su *StateUpdate, mt *MethodTransition) bool {
	switch su.name {
	case "CallSubroutine":
		if len(su.args) != 3 {
			return false
		}
		// CallSubroutine(SubroutineStateMachine, MigrateFunc, SubroutineExitFunc) StateUpdate
		//
		// this step -> SubroutineSM -> SubroutineExitFunc

		mt.Operation = "CallSubroutine"
		if mh := p.getInlineFuncExpr(su.args[1], Migration); mh != "" {
			mt.Migration = mh
			mt.InheritMigration = false
		}
		mt.Transition = p.md.Name + `.` + p.getInlineFuncExpr(su.args[0], 0) + `.` + strconv.Itoa(len(p.md.SubSteps)+1)

		mds := p.buildSubStep(mt.Transition, nil, 0)
		mds.AddMigration(mt.Migration)
		mds.IsSubroutine = true

		exitStep := p.getInlineFuncExpr(su.args[2], Execution) // net exactly an execution, but ok
		mds.AddTransition(MethodTransition{
			Transition: exitStep,
		})

		mt.Migration = ""             // Migration from CallSubroutine is not applied to the caller
		mt.HiddenPropagate = exitStep // all settings applied to the next step after SM return

		return true
	case "Error", "Errorf":
		mt.Operation = "Error"
		mt.Transition = "<stop>"
		mt.InheritMigration = false
		return true

	case "Stay":
		mt.Operation = ""
		return false

	case "Stop":
		mt.Transition = "<stop>"
		mt.InheritMigration = false
		return true

	case "Replace":
		mt.Operation = "Replace"
		mt.Transition = p.getInlineFuncExpr(su.args[0], Construction)
		mt.InheritMigration = false
		return mt.Transition != ""

	case "ReplaceWith":
		mt.Operation = "Replace"
		mt.Transition = p.getInlineFuncExpr(su.args[0], 0)
		mt.InheritMigration = false
		return mt.Transition != ""

	case "ThenRepeatOrElse":
		// unsupported - to be removed
		mt.Operation, mt.DelayedStart = p.buildOperation(su.parent)
		mt.Transition = "<ThenRepeatOrElse>"
		return false

	case "ThenRepeatOrJump":
		mt.Operation, mt.DelayedStart = p.buildOperation(su.parent)
		if len(su.args) == 0 {
			return false
		}
		p.md.AddTransition(*mt) // adds a repeat transition, because mt.Transition is empty
		mt.Transition = p.getInlineFuncExpr(su.args[0], Execution)
		return mt.Transition != ""

	case "ThenRepeatOrJumpExt":
		mt.Operation, mt.DelayedStart = p.buildOperation(su.parent)
		p.md.AddTransition(*mt) // adds a repeat transition, because mt.Transition is empty

	case "Repeat":
		if len(su.args) == 0 {
			return false
		}
		mt.Operation = `Repeat(` + p.shortenArgs(su.args, maxArgLen) + `)`
		return true

	case "RestoreStep":
		mt.WaitTransition = true
		mt.Operation = su.name

	default:
		if strings.HasPrefix(su.name, "Then") {
			switch op := su.parent.name; {
			case op == "Sleep":
				mt.WaitTransition = true
			case strings.HasPrefix(op, "Wait"):
				mt.WaitTransition = true
			}

			mt.Operation, mt.DelayedStart = p.buildOperation(su.parent)
		}

		if strings.HasSuffix(su.name, "Ext") {
			break
		}
		if len(su.args) == 0 { // repeat and similar ops
			return true
		}
		mt.Transition = p.getInlineFuncExpr(su.args[0], Execution)
		return mt.Transition != ""
	}

	if len(su.args) == 0 {
		return false
	}

	mh := ""
	mt.Transition, mh = p.getSlotStepExpr(su.args[0])
	if mh != "" {
		mt.Migration = mh
		mt.InheritMigration = false
	}
	return mt.Transition != ""
}

func (p *ExecTrace) getSlotStepExpr(expr ast.Expr) (transition, migration string) {
	switch op := expr.(type) {
	case *ast.CompositeLit:
		if _, sel := getSelectorOfExpr(op.Type); sel != "SlotStep" {
			break
		}

		for _, el := range op.Elts {
			if kve, ok := el.(*ast.KeyValueExpr); ok {
				switch xkey, key := getSelectorOfExpr(kve.Key); {
				case xkey != "":
				case key == "Transition":
					transition = p.getInlineFuncExpr(kve.Value, Execution)
				case key == "Migration":
					migration = p.getInlineFuncExpr(kve.Value, Migration)
				}
			}
		}
		return
	}

	_, sel := getSelectorOfExpr(expr)
	return "DYNAMIC " + sel, ""
}

func (p *ExecTrace) buildSubStep(name string, args *ast.FieldList, mType MethodType) *MethodDecl {
	md := &MethodDecl{
		SM:    p.md.SM,
		RType: p.md.RType,
		RName: p.md.RName,
		Name:  name,
		MType: mType,
	}

	if mType.HasContextArg() && args != nil {
		if mt, argName := p.fs.findContextArg(args.List); mt == mType {
			md.CtxArg = argName
		}
	}

	if mType.HasStateUpdate() {
		md.UpdateIdx = 1
	}
	p.md.SubSteps = append(p.md.SubSteps, md)
	return md
}

func (p *ExecTrace) getInlineFuncExpr(expr ast.Expr, mType MethodType) string {
	switch op := expr.(type) {
	case *ast.UnaryExpr:
		if mType.HasStateUpdate() {
			break
		}

		if op.Op == token.AND {
			return p.getInlineFuncExpr(op.X, mType)
		}
	case *ast.CompositeLit:
		if mType != 0 {
			break
		}

		x, sel := getSelectorOfExpr(op.Type)
		if x != "" {
			sel = x + `.` + sel + `{}`
		} else {
			sel += `{}`
		}
		mds := p.buildSubStep(sel, nil, mType)
		mds.IsSubroutine = true
		return sel

	case *ast.FuncLit:
		funcName := p.md.Name + `.` + strconv.Itoa(len(p.md.SubSteps)+1)
		mds := p.buildSubStep(funcName, op.Type.Params, mType)
		mds.parseFuncBody(op.Body, p.fs)

		return funcName
	}

	switch x, sel := getSelectorOfExpr(expr); {
	case x != "":
		return x + `.` + sel
	case sel != "nil":
		return sel
	}
	return ""
}

func (p *ExecTrace) buildCondition() string {

	s := ""
	maxLen := maxCondLen

	if len(p.conds) == 1 {
		s = p.shortenCond(p.conds[0], maxLen)
	} else {
		b := strings.Builder{}
		b.WriteString(p.shortenCond(p.conds[0], maxLen))
		for _, c := range p.conds[1:] {
			if max := maxLen - b.Len(); max > 3 {
				cs := p.shortenCond(c, max)
				if len(cs) < max+3 {
					b.WriteString(cs)
					continue
				}
			}
			b.WriteString("...")
			break
		}
		s = b.String()
	}

	s = strconv.Quote(s)
	s = `[` + s[1:len(s)-1] + `]`
	if p.inverted {
		return `!` + s
	}
	return s
}

func (p *ExecTrace) shortenArgs(args []ast.Expr, maxLen int) string {
	if len(args) == 0 {
		return ""
	}
	return p.shortenCond(args[0], maxLen)
}

func (p *ExecTrace) shortenCond(cond ast.Expr, maxLen int) string {
	s := p._shortenCond(cond, maxLen)
	if s != "" {
		return s
	}
	return p.fs.Excerpt(cond.Pos(), cond.End(), maxLen)
}

func (p *ExecTrace) _shortenCond(cond ast.Expr, maxLen int) string {
	switch op := cond.(type) {
	case *ast.SelectorExpr:
		if op.Sel == nil {
			return p._shortenCond(op.X, maxLen)
		}
		s := op.Sel.Name
		if len(s) >= maxLen {
			if op.X != nil {
				return `(...).` + s
			}
			return s
		}
		return p._shortenCond(op.X, maxLen-len(s)-1) + `.` + s
	case *ast.Ident:
		return op.Name
	case *ast.CallExpr:
		return p._shortenCond(op.Fun, maxLen-2) + `()`
	case *ast.BasicLit:
		return op.Value
	case *ast.FuncLit:
		return "func(){}"
	case *ast.CompositeLit:
		return p._shortenCond(op.Type, maxLen-2) + `{}`
	case *ast.ParenExpr:
		return `(` + p._shortenCond(op.X, maxLen-2) + `)`
	case *ast.IndexExpr:
		return p._shortenCond(op.X, maxLen-2) + `[]`
	case *ast.SliceExpr:
		return p._shortenCond(op.X, maxLen-2) + `[]`
	case *ast.TypeAssertExpr:
		return p._shortenCond(op.X, maxLen)
	case *ast.StarExpr:
		return p._shortenCond(op.X, maxLen)
	case *ast.UnaryExpr:
		switch op.Op {
		case token.AND:
			return p._shortenCond(op.X, maxLen)
		case token.XOR:
			return `^` + p._shortenCond(op.X, maxLen-1)
		case token.NOT:
			return `!` + p._shortenCond(op.X, maxLen-1)
		default:
			return p._shortenCond(op.X, maxLen)
		}
	case *ast.BinaryExpr:
		tok := op.Op.String()
		s := tok + p._shortenCond(op.Y, maxLen-1-len(tok))
		if len(s) >= maxLen {
			return `...` + s
		}
		return p._shortenCond(op.X, maxLen-len(s)) + s
	default:
		return "(...)"
	}
}

func (p *ExecTrace) formatUpdateName(su *StateUpdate) string {
	if len(su.args) == 0 && !su.isCall {
		return su.name
	}
	return su.name + `(` + p.shortenArgs(su.args, maxArgLen) + `)`
}

func (p *ExecTrace) buildOperation(su *StateUpdate) (op, adapter string) {
	if !su.HasName() {
		return "", ""
	}

	if su.isContext {
		s := su.name
		for su = su.parent; su.HasName(); su = su.parent {
			if len(su.args) == 0 {
				s = `.` + su.name
			} else {
				s = `.` + p.formatUpdateName(su)
			}
		}
		return s, ""
	}

	op = `DelayedStart`
	switch {
	case su.name == op:
		op = ""
	case su.parent != nil && su.parent.name == op && len(su.args) == 0:
		op = su.name
		su = su.parent
	default:
		return "", ""
	}

	if prep, adapt := p._extractAdapterCall(su); prep != nil {
		prepName := ""
		prepName, adapter = p.getAdapterCallNames(prep, adapt)
		p.md.AddAdapter(adapter)

		return prepName + `.` + op, adapter
	}

	return "", ""
}

func (p *ExecTrace) buildCallChain(su *StateUpdate) string {
	if su == nil {
		return ""
	}
	s := p.formatUpdateName(su)
	for su = su.parent; su.HasName(); su = su.parent {
		s = p.formatUpdateName(su) + `.` + s
	}
	return s
}

func (p *ExecTrace) getAdapterCallNames(start, prep *StateUpdate) (prepName, adapter string) {
	adapter = p.buildCallChain(prep.parent)

	a := prep.parent
	prep.parent = nil
	prepName = p.buildCallChain(start.parent)
	prep.parent = a

	return prepName, adapter
}

func (p *ExecTrace) addAdapterCall(start, prep *StateUpdate) {
	prepName, adapter := p.getAdapterCallNames(start, prep)
	p.md.AddAdapterCall(start.name, prepName, adapter)
}
