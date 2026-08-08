package conflict

import (
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/liufengxi/dbguard/internal/model"
)

const (
	defaultWindowMinutes = 30
	maxWindowMinutes     = 8 * 60
)

var sqlWritePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\balter\s+table\s+([\w."]+)`),
	regexp.MustCompile(`(?i)\bupdate\s+([\w."]+)`),
	regexp.MustCompile(`(?i)\binsert\s+into\s+([\w."]+)`),
	regexp.MustCompile(`(?i)\bdelete\s+from\s+([\w."]+)`),
	regexp.MustCompile(`(?i)\btruncate(?:\s+table)?\s+([\w."]+)`),
}

type resource struct {
	kind string
	name string
}

// Detect builds an explainable, deterministic conflict snapshot.
func Detect(changes []model.ChangeRequest, applications []model.Application, from, to time.Time) model.ConflictRadar {
	if from.IsZero() {
		from = time.Now().Add(-24 * time.Hour)
	}
	if to.IsZero() || !to.After(from) {
		to = from.Add(8 * 24 * time.Hour)
	}
	radar := model.ConflictRadar{
		GeneratedAt: time.Now(), WindowStart: from, WindowEnd: to,
		SeverityDistribution: map[model.RiskLevel]int{
			model.RiskHigh: 0, model.RiskMedium: 0, model.RiskLow: 0,
		},
		Changes: []model.ConflictChangeSummary{}, Conflicts: []model.ChangeConflict{},
	}
	appByID := make(map[string]model.Application, len(applications))
	for _, app := range applications {
		appByID[app.ID] = app
	}
	active := make([]model.ChangeRequest, 0, len(changes))
	for _, change := range changes {
		if terminal(change.Status) || change.PlannedAt.IsZero() ||
			change.PlannedAt.Before(from) || change.PlannedAt.After(to) {
			continue
		}
		active = append(active, change)
	}
	sort.Slice(active, func(i, j int) bool {
		if active[i].PlannedAt.Equal(active[j].PlannedAt) {
			return active[i].ID < active[j].ID
		}
		return active[i].PlannedAt.Before(active[j].PlannedAt)
	})
	for _, change := range active {
		radar.Changes = append(radar.Changes, summary(change))
	}
	radar.PlannedChanges = len(active)
	affected := map[string]struct{}{}
	for i := 0; i < len(active); i++ {
		for j := i + 1; j < len(active); j++ {
			left, right := active[i], active[j]
			if !sameEnvironment(left.Environment, right.Environment) {
				continue
			}
			start, end, ok := overlap(left, right)
			if !ok {
				continue
			}
			reasons, score := reasonsFor(left, right, appByID)
			if len(reasons) == 0 {
				continue
			}
			if left.Risk == model.RiskHigh {
				score += 5
			}
			if right.Risk == model.RiskHigh {
				score += 5
			}
			if end.Sub(start) >= time.Hour {
				score += 5
			}
			if score > 100 {
				score = 100
			}
			severity := severityFor(score)
			radar.Conflicts = append(radar.Conflicts, model.ChangeConflict{
				ID: conflictID(left.ID, right.ID), Severity: severity, Score: score,
				ChangeA: summary(left), ChangeB: summary(right),
				OverlapStart: start, OverlapEnd: end,
				OverlapMinutes: int(end.Sub(start).Minutes()),
				Reasons:        reasons, Recommendation: recommendation(reasons),
			})
			radar.SeverityDistribution[severity]++
			if severity == model.RiskHigh {
				radar.HighRiskCount++
			}
			affected[left.ApplicationID] = struct{}{}
			affected[right.ApplicationID] = struct{}{}
		}
	}
	sort.Slice(radar.Conflicts, func(i, j int) bool {
		if radar.Conflicts[i].Score == radar.Conflicts[j].Score {
			return radar.Conflicts[i].OverlapStart.Before(radar.Conflicts[j].OverlapStart)
		}
		return radar.Conflicts[i].Score > radar.Conflicts[j].Score
	})
	radar.ConflictCount = len(radar.Conflicts)
	radar.AffectedApplications = len(affected)
	return radar
}

func terminal(status model.ChangeStatus) bool {
	return status == model.StatusCheckFailed ||
		status == model.StatusRejected ||
		status == model.StatusCompleted
}

func summary(change model.ChangeRequest) model.ConflictChangeSummary {
	return model.ConflictChangeSummary{
		ID: change.ID, Title: change.Title,
		ApplicationID: change.ApplicationID, ApplicationName: change.ApplicationName,
		Environment: change.Environment, Status: change.Status, Risk: change.Risk,
		PlannedAt: change.PlannedAt, WindowEnd: windowEnd(change),
	}
}

func windowEnd(change model.ChangeRequest) time.Time {
	minutes := change.ReleasePlan.ObservationMinutes
	if minutes < defaultWindowMinutes {
		minutes = defaultWindowMinutes
	}
	if minutes > maxWindowMinutes {
		minutes = maxWindowMinutes
	}
	return change.PlannedAt.Add(time.Duration(minutes) * time.Minute)
}

func overlap(left, right model.ChangeRequest) (time.Time, time.Time, bool) {
	start := left.PlannedAt
	if right.PlannedAt.After(start) {
		start = right.PlannedAt
	}
	end := windowEnd(left)
	if candidate := windowEnd(right); candidate.Before(end) {
		end = candidate
	}
	return start, end, end.After(start)
}

func reasonsFor(left, right model.ChangeRequest, apps map[string]model.Application) ([]model.ConflictReason, int) {
	reasons := make([]model.ConflictReason, 0, 4)
	score := 0
	if left.ApplicationID != "" && left.ApplicationID == right.ApplicationID {
		reasons = append(reasons, model.ConflictReason{
			Kind: "SAME_APPLICATION", Label: "同一应用并发发布", Resource: left.ApplicationName,
		})
		score = max(score, 90)
	}
	leftResources, rightResources := resources(left), resources(right)
	for key, item := range leftResources {
		if _, ok := rightResources[key]; !ok {
			continue
		}
		label, weight := resourceLabel(item.kind)
		reasons = append(reasons, model.ConflictReason{
			Kind: "SHARED_" + item.kind, Label: label, Resource: item.name,
		})
		score = max(score, weight)
	}
	if dependency, ok := dependencyRelation(left.ApplicationID, right.ApplicationID, apps); ok {
		reasons = append(reasons, model.ConflictReason{
			Kind: "APPLICATION_DEPENDENCY", Label: "上下游依赖同时变更", Resource: dependency,
		})
		score = max(score, 65)
	}
	sort.Slice(reasons, func(i, j int) bool {
		if reasons[i].Kind == reasons[j].Kind {
			return reasons[i].Resource < reasons[j].Resource
		}
		return reasons[i].Kind < reasons[j].Kind
	})
	return reasons, score
}

func resources(change model.ChangeRequest) map[string]resource {
	items := map[string]resource{}
	add := func(kind, name string) {
		name = normalizeResource(name)
		if name != "" {
			items[kind+":"+name] = resource{kind: kind, name: name}
		}
	}
	for _, table := range sqlTables(change.SQL) {
		add("DATABASE", table)
	}
	for _, artifact := range change.Artifacts {
		switch strings.ToUpper(string(artifact.Kind)) {
		case "DATABASE":
			for _, table := range sqlTables(artifact.Content) {
				add("DATABASE", table)
			}
		case "CONFIG":
			prefix := firstNonEmpty(artifact.Source, artifact.Name)
			for _, key := range configKeys(artifact.Content) {
				add("CONFIG", prefix+":"+key)
			}
		case "KUBERNETES":
			for _, name := range kubernetesObjects(artifact.Content) {
				add("KUBERNETES", name)
			}
		default:
			if looksLikePath(artifact.Source) {
				add("ARTIFACT", artifact.Source)
			}
		}
	}
	return items
}

func sqlTables(content string) []string {
	found := map[string]struct{}{}
	for _, pattern := range sqlWritePatterns {
		for _, match := range pattern.FindAllStringSubmatch(content, -1) {
			if len(match) > 1 {
				name := normalizeResource(match[1])
				if name != "" {
					found[name] = struct{}{}
				}
			}
		}
	}
	return sortedKeys(found)
}

func configKeys(content string) []string {
	found := map[string]struct{}{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") ||
			strings.HasPrefix(line, "//") || strings.HasPrefix(line, "-") {
			continue
		}
		index := strings.IndexAny(line, ":=")
		if index <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:index])
		if key != "" && len(key) <= 96 {
			found[strings.ToLower(key)] = struct{}{}
		}
	}
	return sortedKeys(found)
}

func kubernetesObjects(content string) []string {
	found := map[string]struct{}{}
	var kind, name, namespace string
	flush := func() {
		if kind != "" && name != "" {
			value := firstNonEmpty(namespace, "default") + "/" + kind + "/" + name
			found[strings.ToLower(value)] = struct{}{}
		}
		kind, name, namespace = "", "", ""
	}
	inMetadata := false
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "---" {
			flush()
			inMetadata = false
			continue
		}
		if strings.HasPrefix(line, "kind:") {
			kind = strings.TrimSpace(strings.TrimPrefix(line, "kind:"))
			continue
		}
		if line == "metadata:" {
			inMetadata = true
			continue
		}
		if inMetadata && strings.HasPrefix(line, "name:") && name == "" {
			name = strings.TrimSpace(strings.TrimPrefix(line, "name:"))
			continue
		}
		if inMetadata && strings.HasPrefix(line, "namespace:") {
			namespace = strings.TrimSpace(strings.TrimPrefix(line, "namespace:"))
		}
	}
	flush()
	return sortedKeys(found)
}

func dependencyRelation(leftID, rightID string, apps map[string]model.Application) (string, bool) {
	left, leftOK := apps[leftID]
	right, rightOK := apps[rightID]
	if !leftOK || !rightOK {
		return "", false
	}
	if contains(left.Dependencies, rightID) {
		return right.Name, true
	}
	if contains(right.Dependencies, leftID) {
		return left.Name, true
	}
	return "", false
}

func recommendation(reasons []model.ConflictReason) string {
	for _, reason := range reasons {
		if reason.Kind == "SAME_APPLICATION" || strings.HasPrefix(reason.Kind, "SHARED_") {
			return "建议将两个变更错峰，前一变更完成并观察稳定后再执行后一变更。"
		}
	}
	return "建议先发布上游并完成健康检查，再放行下游变更。"
}

func resourceLabel(kind string) (string, int) {
	switch kind {
	case "DATABASE":
		return "共享数据库对象", 88
	case "KUBERNETES":
		return "共享 Kubernetes 工作负载", 82
	case "CONFIG":
		return "共享配置项", 76
	default:
		return "共享发布制品", 70
	}
}

func severityFor(score int) model.RiskLevel {
	if score >= 80 {
		return model.RiskHigh
	}
	if score >= 60 {
		return model.RiskMedium
	}
	return model.RiskLow
}

func conflictID(left, right string) string {
	if left > right {
		left, right = right, left
	}
	return "conf_" + left + "_" + right
}

func sameEnvironment(left, right string) bool {
	return strings.EqualFold(strings.TrimSpace(left), strings.TrimSpace(right))
}

func normalizeResource(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "(),;")
	return strings.ToLower(value)
}

func looksLikePath(value string) bool {
	value = strings.TrimSpace(value)
	return strings.Contains(value, "/") ||
		strings.ContainsRune(value, rune(92)) ||
		strings.Contains(value, ".")
}

func contains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func sortedKeys(items map[string]struct{}) []string {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}
