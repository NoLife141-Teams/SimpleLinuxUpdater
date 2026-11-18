package main

import "github.com/gin-gonic/gin"

type operatorApplicationShellView struct {
	Role          string
	ActiveSection string
	PageLabel     string
}

func newOperatorApplicationShellView(activeSection, pageLabel string) operatorApplicationShellView {
	return operatorApplicationShellView{
		ActiveSection: activeSection,
		PageLabel:     pageLabel,
	}
}

func operatorShellForContext(c *gin.Context, activeSection, pageLabel string) operatorApplicationShellView {
	view := newOperatorApplicationShellView(activeSection, pageLabel)
	if p, ok := principalForContext(c); ok {
		view.Role = p.Account.Role
	}
	return view
}
