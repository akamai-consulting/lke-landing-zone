package health

import "testing"

func TestResourceStatus(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want Category
	}{
		{
			name: "ready deployment is OK",
			raw: `{
				"apiVersion":"apps/v1","kind":"Deployment",
				"metadata":{"namespace":"harbor","name":"harbor-core","generation":1},
				"spec":{"replicas":3},
				"status":{"observedGeneration":1,"replicas":3,"updatedReplicas":3,"readyReplicas":3,"availableReplicas":3,
					"conditions":[{"type":"Available","status":"True"},{"type":"Progressing","status":"True","reason":"NewReplicaSetAvailable"}]}
			}`,
			want: CatOK,
		},
		{
			name: "under-replicated deployment is pending",
			raw: `{
				"apiVersion":"apps/v1","kind":"Deployment",
				"metadata":{"namespace":"harbor","name":"harbor-core","generation":1},
				"spec":{"replicas":3},
				"status":{"observedGeneration":1,"replicas":3,"updatedReplicas":3,"readyReplicas":1,"availableReplicas":1,
					"conditions":[{"type":"Available","status":"False"},{"type":"Progressing","status":"True","reason":"ReplicaSetUpdated"}]}
			}`,
			want: CatPending,
		},
		{
			name: "deployment past its progress deadline fails",
			raw: `{
				"apiVersion":"apps/v1","kind":"Deployment",
				"metadata":{"namespace":"harbor","name":"harbor-core","generation":2},
				"spec":{"replicas":3},
				"status":{"observedGeneration":2,"replicas":3,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1,
					"conditions":[{"type":"Progressing","status":"False","reason":"ProgressDeadlineExceeded","message":"exceeded its progress deadline"}]}
			}`,
			want: CatFail,
		},
		{
			name: "deployment that has not observed its spec is pending",
			raw: `{
				"apiVersion":"apps/v1","kind":"Deployment",
				"metadata":{"namespace":"harbor","name":"harbor-core","generation":3},
				"spec":{"replicas":3},
				"status":{"observedGeneration":2,"replicas":3,"updatedReplicas":3,"readyReplicas":3,"availableReplicas":3}
			}`,
			want: CatPending,
		},
		{
			name: "partially-ready statefulset is pending",
			raw: `{
				"apiVersion":"apps/v1","kind":"StatefulSet",
				"metadata":{"namespace":"openbao","name":"openbao","generation":1},
				"spec":{"replicas":3},
				"status":{"observedGeneration":1,"replicas":3,"readyReplicas":1,"currentReplicas":3,"updatedReplicas":3,
					"currentRevision":"openbao-abc","updateRevision":"openbao-abc"}
			}`,
			want: CatPending,
		},
		{
			name: "fully-ready statefulset is OK",
			raw: `{
				"apiVersion":"apps/v1","kind":"StatefulSet",
				"metadata":{"namespace":"openbao","name":"openbao","generation":1},
				"spec":{"replicas":3},
				"status":{"observedGeneration":1,"replicas":3,"readyReplicas":3,"currentReplicas":3,"updatedReplicas":3,
					"currentRevision":"openbao-abc","updateRevision":"openbao-abc"}
			}`,
			want: CatOK,
		},
		{
			name: "running ready pod is OK",
			raw: `{
				"apiVersion":"v1","kind":"Pod",
				"metadata":{"namespace":"harbor","name":"harbor-core-0"},
				"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}
			}`,
			want: CatOK,
		},
		{
			// Documents a kstatus semantic gotcha: a Pod in a terminal phase
			// (Failed/Succeeded) is reported as Current — kstatus considers it
			// "done, won't change", not a failure. This is why ResourceStatus is
			// adopted for controller rollouts (Deployment/StatefulSet), where a
			// failed Pod surfaces as an under-replicated controller, and NOT used
			// to gate on bare Pods directly.
			name: "terminal failed pod reports current (kstatus semantics)",
			raw: `{
				"apiVersion":"v1","kind":"Pod",
				"metadata":{"namespace":"harbor","name":"harbor-core-0"},
				"status":{"phase":"Failed","conditions":[{"type":"Ready","status":"False"}]}
			}`,
			want: CatOK,
		},
		{
			name: "unparseable json fails",
			raw:  `{not json`,
			want: CatFail,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, label := ResourceStatus([]byte(tt.raw))
			if got != tt.want {
				t.Fatalf("ResourceStatus() category = %v, want %v (label=%q)", got, tt.want, label)
			}
		})
	}
}
