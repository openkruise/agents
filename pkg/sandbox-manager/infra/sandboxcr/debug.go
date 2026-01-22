package sandboxcr

type DebugInfo struct {
	Pending int
	Claimed int
	Total   int
}

func (i *Infra) LoadDebugInfo() map[string]any {
	infos := make(map[string]any)
	i.templates.Range(func(key, value any) bool {
		infos[key.(string)] = map[string]any{
			"namespaces": value.(int32),
		}
		return true
	})
	return infos
}
