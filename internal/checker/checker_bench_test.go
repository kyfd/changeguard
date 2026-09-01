package checker

import (
	"fmt"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

func BenchmarkKubernetesRuleScan(b *testing.B) {
	policies := model.DefaultRiskPolicies(time.Now())
	context := Context{Environment: "生产环境", ChangeType: "K8S"}
	for _, size := range []int{100, 1000, 10000} {
		input := ReleaseInput{Artifacts: kubernetesArtifacts(size), RollbackPlan: "helm rollback orders 1"}
		b.Run(fmt.Sprintf("artifacts/%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				result := CheckReleaseWithPolicies(input, context, policies)
				if len(result.Findings) == 0 {
					b.Fatal("expected Kubernetes findings")
				}
			}
		})
	}
}

func kubernetesArtifacts(count int) []model.ChangeArtifact {
	items := make([]model.ChangeArtifact, count)
	for index := 0; index < count; index++ {
		items[index] = model.ChangeArtifact{
			Kind: model.ArtifactKubernetes,
			Name: fmt.Sprintf("deploy-%d.yaml", index),
			Content: fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: orders-%d
spec:
  replicas: 1
  template:
    spec:
      containers:
        - name: app
          image: registry.example.com/orders:latest
          securityContext:
            privileged: true
            runAsUser: 0
          volumeMounts:
            - name: host
              mountPath: /host
      volumes:
        - name: host
          hostPath:
            path: /
`, index),
		}
	}
	return items
}
