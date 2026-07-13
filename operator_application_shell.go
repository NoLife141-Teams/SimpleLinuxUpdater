package main

type operatorApplicationShellView struct {
	ActiveSection string
	PageLabel     string
}

func newOperatorApplicationShellView(activeSection, pageLabel string) operatorApplicationShellView {
	return operatorApplicationShellView{
		ActiveSection: activeSection,
		PageLabel:     pageLabel,
	}
}
