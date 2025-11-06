package k8s

type DebugInfo struct {
	Pending int
	Running int
	Paused  int
}

func (i *Infra) LoadDebugInfo() map[string]any {
	infos := make(map[string]any)
	i.Pools.Range(func(key, value any) bool {
		pool := value.(*Pool)
		infos[pool.template.Name] = DebugInfo{
			Pending: int(pool.pending.Load()),
			Running: int(pool.running.Load()),
			Paused:  int(pool.paused.Load()),
		}
		return true
	})
	infos["infra"] = "k8s"
	return infos
}
