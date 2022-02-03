package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/k8sbykeshed/k8s-service-validator/entities"
	"github.com/k8sbykeshed/k8s-service-validator/entities/kubernetes"
	"github.com/k8sbykeshed/k8s-service-validator/matrix"
	"github.com/k8sbykeshed/k8s-service-validator/tools"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

var (
	ctx   = context.Background()
	delay = 5 * time.Second
)

// TestBasicService starts up the basic Kubernetes services available
func TestBasicService(t *testing.T) { // nolint
	pods := model.AllPods()
	var services kubernetes.Services

	featureClusterIP := features.New("ClusterIP").WithLabel("type", "cluster_ip").
		Setup(func(context.Context, *testing.T, *envconf.Config) context.Context {
			services = make(kubernetes.Services, len(pods))
			for _, pod := range pods {
				var (
					err       error
					result    bool
					clusterIP string
				)
				// Create a kubernetes service based in the service spec
				clusterSvc := pod.ClusterIPService()
				var service kubernetes.ServiceBase = kubernetes.NewService(manager.GetClientSet(), clusterSvc)
				if _, err := service.Create(); err != nil {
					t.Error(err)
				}

				// wait for final status
				if result, err = service.WaitForEndpoint(); err != nil || !result {
					t.Error(errors.New("no endpoint available"))
				}

				if clusterIP, err = service.WaitForClusterIP(); err != nil || clusterIP == "" {
					t.Error(errors.New("no cluster IP available"))
				}
				pod.SetClusterIP(clusterIP)
				services = append(services, service.(*kubernetes.Service))
			}
			return ctx
		}).
		Teardown(func(context.Context, *testing.T, *envconf.Config) context.Context {
			tools.ResetTestBoard(t, services, model)
			return ctx
		}).
		Assess("should be reachable via cluster IP", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			zap.L().Info("Testing ClusterIP with TCP protocol.")
			reachabilityTCP := matrix.NewReachability(pods, true)
			tools.MustNoWrong(matrix.ValidateOrFail(manager, model, &matrix.TestCase{
				ToPort: 80, Protocol: v1.ProtocolTCP, Reachability: reachabilityTCP, ServiceType: entities.ClusterIP,
			}, false, false), t)

			zap.L().Info("Testing ClusterIP with UDP protocol.")
			reachabilityUDP := matrix.NewReachability(pods, true)
			tools.MustNoWrong(matrix.ValidateOrFail(manager, model, &matrix.TestCase{
				ToPort: 80, Protocol: v1.ProtocolUDP, Reachability: reachabilityUDP, ServiceType: entities.ClusterIP,
			}, false, false), t)
			return ctx
		}).Feature()

	// Test session affinity clientIP
	podsWithAffinity := make([]*entities.Pod, 2)
	featureSessionAffinity := features.New("SessionAffinity").WithLabel("type", "cluster_ip_sessionAffinity").
		Setup(func(context.Context, *testing.T, *envconf.Config) context.Context {
			services = make(kubernetes.Services, len(pods))
			// add new label to two pods, pod-3 and pod-4
			labelKey := "app"
			labelValue := "test-session-affinity"
			// create new pods with session affinity labels
			for i := 0; i < len(podsWithAffinity); i++ {
				newPod := &entities.Pod{
					Name:      fmt.Sprintf("paf-%d", i),
					Namespace: namespace,
					Containers: []*entities.Container{
						{Port: 80, Protocol: v1.ProtocolTCP},
						{Port: 81, Protocol: v1.ProtocolTCP},
					},
					Labels: map[string]string{labelKey: labelValue},
				}
				err := manager.InitializePod(newPod)
				if err != nil {
					t.Fatal(err)
				}
				model.AddPod(newPod, namespace)
				podsWithAffinity[i] = newPod
			}
			// create cluster IP service with the new label and session affinity: clientIP
			_, service, clusterIP, err := matrix.CreateServiceFromTemplate(manager.GetClientSet(), entities.ServiceTemplate{
				Name:            "service-session-affinity",
				Namespace:       namespace,
				Selector:        map[string]string{labelKey: labelValue},
				SessionAffinity: true,
				ProtocolPorts: []entities.ProtocolPortPair{
					{Protocol: v1.ProtocolTCP, Port: 80},
					{Protocol: v1.ProtocolTCP, Port: 81},
				},
			})
			if err != nil {
				t.Error(err)
			}

			pods = model.AllPods()
			for _, p := range pods {
				p.SetClusterIP(clusterIP)
			}

			services = []*kubernetes.Service{service.(*kubernetes.Service)}
			return ctx
		}).
		Teardown(func(context.Context, *testing.T, *envconf.Config) context.Context {
			for _, p := range podsWithAffinity {
				err := model.RemovePod(p.Name, namespace)
				if err != nil {
					zap.L().Debug(err.Error())
				}
				if err := manager.DeletePod(p.Name, p.Namespace); err != nil {
					t.Fatal(err)
				}
			}
			tools.ResetTestBoard(t, services, model)
			return ctx
		}).
		Assess("should always reach to same target pods.", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			zap.L().Info("Testing ClusterIP session affinity to one pod.")

			clusterIPWithSessionAffinity := podsWithAffinity[0].GetClusterIP()
			// setup affinity
			fromToPeer := map[string]string{}
			for _, p := range pods {
				connected, endpoint, connectCmd, err := manager.ProbeConnectivityWithNc(namespace, p.Name, p.Containers[0].Name, clusterIPWithSessionAffinity, v1.ProtocolTCP, 80)
				if err != nil {
					t.Error(errors.Wrapf(err, "failed to establish affinity with cmd: %v", connectCmd))
				}
				if !connected {
					t.Error(errors.New("failed to connect the ClusterIP service with sessionAffinity"))
				}
				fromToPeer[p.Name] = endpoint
			}

			// to validate if affiliation applies to same port
			zap.L().Info(fmt.Sprintf("Testing connections to same ports, try multiple times to affirm the affiliation, should use same from/to peers: %v", fromToPeer))
			zap.L().Debug(fmt.Sprintf("Session affinity peers: %v", fromToPeer))
			const connectTimes = 3
			for i := 0; i < connectTimes; i++ {
				zap.L().Info("Connection via port 80")
				reachabilityPort80 := matrix.NewReachability(pods, false)
				for from, to := range fromToPeer {
					reachabilityPort80.ExpectPeer(&matrix.Peer{Namespace: namespace, Pod: from}, &matrix.Peer{Namespace: namespace, Pod: to}, true)
				}
				tools.MustNoWrong(matrix.ValidateOrFail(manager, model, &matrix.TestCase{
					ToPort: 80, Protocol: v1.ProtocolTCP, Reachability: reachabilityPort80, ServiceType: entities.ClusterIP,
				}, false, true), t)
			}

			// to validate if affiliation applies to other ports
			zap.L().Info(fmt.Sprintf("Testing connections to different ports of sesson affinity service, should use same from/to peers: %v", fromToPeer))
			zap.L().Info("Connection via port 81")
			reachabilityPort81 := matrix.NewReachability(pods, false)
			for from, to := range fromToPeer {
				reachabilityPort81.ExpectPeer(&matrix.Peer{Namespace: namespace, Pod: from}, &matrix.Peer{Namespace: namespace, Pod: to}, true)
			}
			wrongNum := matrix.ValidateOrFail(manager, model, &matrix.TestCase{
				ToPort: 81, Protocol: v1.ProtocolTCP, Reachability: reachabilityPort81, ServiceType: entities.ClusterIP,
			}, false, true)
			if wrongNum > 0 {
				zap.L().Warn("Reproduced issue for testing session affinity https://github.com/kubernetes/kubernetes/issues/103000, " +
					"same client reach to different target pods via different ports. Warning as issue still open...")
			}
			return ctx
		}).Feature()

	// Test endless service
	featureEndlessService := features.New("EndlessService").WithLabel("type", "cluster_ip_endless").
		Setup(func(context.Context, *testing.T, *envconf.Config) context.Context {
			services = make(kubernetes.Services, len(pods))
			var endlessServicePort int32 = 80
			// Create a service with no endpoints
			_, service, clusterIP, err := matrix.CreateServiceFromTemplate(manager.GetClientSet(), entities.ServiceTemplate{
				Name:          "endless",
				Namespace:     namespace,
				ProtocolPorts: []entities.ProtocolPortPair{{Protocol: v1.ProtocolTCP, Port: endlessServicePort}},
			})
			if err != nil {
				t.Error(err)
			}

			for _, pod := range pods {
				pod.SetClusterIP(clusterIP)
				pod.SetToPort(endlessServicePort)
			}

			services = []*kubernetes.Service{service.(*kubernetes.Service)}
			return ctx
		}).
		Teardown(func(context.Context, *testing.T, *envconf.Config) context.Context {
			tools.ResetTestBoard(t, services, model)
			return ctx
		}).
		Assess("should not be reachable via endless service", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			zap.L().Info("Testing Endless service.")
			reachability := matrix.NewReachability(model.AllPods(), true)
			reachability.ExpectPeer(&matrix.Peer{Namespace: namespace}, &matrix.Peer{Namespace: namespace}, false)
			tools.MustNoWrong(matrix.ValidateOrFail(manager, model, &matrix.TestCase{
				Protocol: v1.ProtocolTCP, Reachability: reachability, ServiceType: entities.ClusterIP,
			}, false, false), t)
			return ctx
		}).Feature()

	// Test hairpin
	featureHairpin := features.New("Hairpin").WithLabel("type", "cluster_ip_hairpin").
		Setup(func(context.Context, *testing.T, *envconf.Config) context.Context {
			services = make(kubernetes.Services, len(pods))
			// Create a clusterIP service
			serviceName, service, _, err := matrix.CreateServiceFromTemplate(manager.GetClientSet(), entities.ServiceTemplate{
				Name:          "hairpin",
				Namespace:     namespace,
				ProtocolPorts: []entities.ProtocolPortPair{{Protocol: pods[0].Containers[0].Protocol, Port: 80}},
			})
			if err != nil {
				t.Error(err)
			}

			pods[0].SetServiceName(serviceName)

			services = []*kubernetes.Service{service.(*kubernetes.Service)}
			return ctx
		}).
		Teardown(func(context.Context, *testing.T, *envconf.Config) context.Context {
			tools.ResetTestBoard(t, services, model)
			return ctx
		}).
		Assess("should be reachable for hairpin", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			zap.L().Info("Testing hairpin.")
			reachability := matrix.NewReachability(model.AllPods(), true)
			reachability.ExpectPeer(&matrix.Peer{Namespace: namespace}, &matrix.Peer{Namespace: namespace, Pod: pods[0].Name}, true)
			tools.MustNoWrong(matrix.ValidateOrFail(manager, model, &matrix.TestCase{
				ToPort: 80, Protocol: v1.ProtocolTCP, Reachability: reachability, ServiceType: entities.ClusterIP,
			}, false, false), t)
			return ctx
		}).Feature()

	featureNodePort := features.New("NodePort").WithLabel("type", "node_port").
		Setup(func(context.Context, *testing.T, *envconf.Config) context.Context {
			services = make(kubernetes.Services, len(pods))
			for _, pod := range pods {
				clusterSvc := pod.NodePortService()

				// Create a kubernetes service based in the service spec
				var service kubernetes.ServiceBase = kubernetes.NewService(manager.GetClientSet(), clusterSvc)
				if _, err := service.Create(); err != nil {
					t.Error(err)
				}

				// Wait for final status
				result, err := service.WaitForEndpoint()
				if err != nil || !result {
					t.Error(errors.New("no endpoint available"))
				}

				nodePort, err := service.WaitForNodePort()
				if err != nil {
					t.Error(err)
				}

				// required for wait complete ip rules creation
				time.Sleep(delay)

				// Set pod specification on entity model
				pod.SetToPort(nodePort)
				services = append(services, service.(*kubernetes.Service))
			}
			return ctx
		}).
		Teardown(func(context.Context, *testing.T, *envconf.Config) context.Context {
			tools.ResetTestBoard(t, services, model)
			return ctx
		}).
		Assess("should reachable on node port TCP and UDP", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			zap.L().Info("Testing NodePort with TCP protocol.")
			reachabilityTCP := matrix.NewReachability(pods, true)
			tools.MustNoWrong(matrix.ValidateOrFail(manager, model, &matrix.TestCase{
				Protocol: v1.ProtocolTCP, Reachability: reachabilityTCP, ServiceType: entities.NodePort,
			}, false, false), t)

			zap.L().Info("Testing NodePort with UDP protocol.")
			reachabilityUDP := matrix.NewReachability(pods, true)
			tools.MustNoWrong(matrix.ValidateOrFail(manager, model, &matrix.TestCase{
				Protocol: v1.ProtocolUDP, Reachability: reachabilityUDP, ServiceType: entities.NodePort,
			}, false, false), t)
			return ctx
		}).Feature()

	featureLoadBalancer := features.New("LoadBalancer").WithLabel("type", "load_balancer").
		Setup(func(context.Context, *testing.T, *envconf.Config) context.Context {
			services = make(kubernetes.Services, len(pods))
			for _, pod := range pods {
				var (
					err error
					ips []entities.ExternalIP
				)
				// Create a load balancer with TCP/UDP ports, based in the service spec
				serviceTCP := kubernetes.NewService(manager.GetClientSet(), pod.LoadBalancerServiceByProtocol(v1.ProtocolTCP))
				if _, err := serviceTCP.Create(); err != nil {
					t.Error(err)
				}
				serviceUDP := kubernetes.NewService(manager.GetClientSet(), pod.LoadBalancerServiceByProtocol(v1.ProtocolUDP))
				if _, err := serviceUDP.Create(); err != nil {
					t.Error(err)
				}

				// Wait for final status
				result, err := serviceTCP.WaitForEndpoint()
				if err != nil || !result {
					t.Error(errors.New("no endpoint available"))
				}

				result, err = serviceUDP.WaitForEndpoint()
				if err != nil || !result {
					t.Error(errors.New("no endpoint available"))
				}

				ipsForTCP, err := serviceTCP.WaitForExternalIP()
				if err != nil {
					t.Error(err)
				}
				ips = append(ips, entities.NewExternalIPs(ipsForTCP, v1.ProtocolTCP)...)

				ipsForUDP, err := serviceUDP.WaitForExternalIP()
				if err != nil {
					t.Error(err)
				}
				ips = append(ips, entities.NewExternalIPs(ipsForUDP, v1.ProtocolUDP)...)

				if len(ips) == 0 {
					t.Error(errors.New("invalid external UDP IPs setup"))
				}

				// Set pod specification on entity model
				pod.SetToPort(80)
				pod.SetExternalIPs(ips)

				services = append(services, serviceTCP, serviceUDP)
			}
			return ctx
		}).
		Teardown(func(context.Context, *testing.T, *envconf.Config) context.Context {
			tools.ResetTestBoard(t, services, model)
			return ctx
		}).
		Assess("should be reachable via load balancer", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			zap.L().Info("Creating load balancer with TCP protocol")
			reachabilityTCP := matrix.NewReachability(pods, true)
			tools.MustNoWrong(matrix.ValidateOrFail(manager, model, &matrix.TestCase{
				Protocol: v1.ProtocolTCP, Reachability: reachabilityTCP, ServiceType: entities.LoadBalancer,
			}, false, false), t)

			zap.L().Info("Creating Loadbalancer with UDP protocol")
			reachabilityUDP := matrix.NewReachability(pods, true)
			tools.MustNoWrong(matrix.ValidateOrFail(manager, model, &matrix.TestCase{
				Protocol: v1.ProtocolUDP, Reachability: reachabilityUDP, ServiceType: entities.LoadBalancer,
			}, false, false), t)
			return ctx
		}).Feature()

	testenv.Test(t, featureClusterIP, featureNodePort, featureLoadBalancer, featureEndlessService, featureHairpin, featureSessionAffinity)
}

func TestExternalService(t *testing.T) {
	const (
		domain = "example.com"
	)

	pods := model.AllPods()
	var services kubernetes.Services

	// Create a node port traffic local service for pod-1 only
	// and share the NodePort with all other pods, the test is using
	// the same port via different nodes IPs (where each pod is scheduled)
	featureNodePortLocal := features.New("NodePort Traffic Local").WithLabel("type", "node_port_traffic_local").
		Setup(func(context.Context, *testing.T, *envconf.Config) context.Context {
			services = make(kubernetes.Services, len(pods))
			// Create a kubernetes service based in the service spec
			var service kubernetes.ServiceBase = kubernetes.NewService(manager.GetClientSet(), pods[0].NodePortLocalService())
			if _, err := service.Create(); err != nil {
				t.Error(err)
			}

			// Wait for final status
			if _, err := service.WaitForClusterIP(); err != nil {
				t.Error(errors.New("no clusterIP available"))
			}

			if _, err := service.WaitForEndpoint(); err != nil {
				t.Error(errors.New("no endpoint available"))
			}

			nodePort, err := service.WaitForNodePort()
			if err != nil {
				t.Error(err)
			}

			// required for wait complete ip rules creation
			time.Sleep(delay)

			// Set pod specification on entity model
			for _, pod := range pods {
				pod.SetToPort(nodePort)
			}

			zap.L().Debug("Nodeport for traffic local policy.", zap.Int32("nodeport", nodePort))

			services = append(services, service.(*kubernetes.Service))
			return ctx
		}).
		Teardown(func(context.Context, *testing.T, *envconf.Config) context.Context {
			tools.ResetTestBoard(t, services, model)
			return ctx
		}).
		Assess("should be reachable via NodePortLocal k8s service", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			zap.L().Info("Testing NodePortLocal with TCP protocol.")
			reachabilityTCP := matrix.NewReachability(pods, false)
			reachabilityTCP.ExpectPeer(&matrix.Peer{Namespace: namespace}, &matrix.Peer{Namespace: namespace, Pod: "pod-1"}, true)
			tools.MustNoWrong(matrix.ValidateOrFail(manager, model, &matrix.TestCase{
				Protocol: v1.ProtocolTCP, Reachability: reachabilityTCP, ServiceType: entities.NodePort,
			}, true, false), t)

			zap.L().Info("Testing NodePortLocal with UDP protocol.")
			reachabilityUDP := matrix.NewReachability(pods, false)
			reachabilityUDP.ExpectPeer(&matrix.Peer{Namespace: namespace}, &matrix.Peer{Namespace: namespace, Pod: "pod-1"}, true)
			tools.MustNoWrong(matrix.ValidateOrFail(manager, model, &matrix.TestCase{
				Protocol: v1.ProtocolUDP, Reachability: reachabilityUDP, ServiceType: entities.NodePort,
			}, true, false), t)
			return ctx
		}).Feature()

	featureExternal := features.New("External Service").WithLabel("type", "external").
		Setup(func(context.Context, *testing.T, *envconf.Config) context.Context {
			services = make(kubernetes.Services, len(pods))
			for _, pod := range pods {
				// Create a kubernetes service based in the service spec
				var service kubernetes.ServiceBase = kubernetes.NewService(manager.GetClientSet(), pod.ExternalNameService(domain))
				k8sSvc, err := service.Create()
				if err != nil {
					t.Error(err)
				}

				pod.SetServiceName(k8sSvc.Name)
				services = append(services, service.(*kubernetes.Service))
			}
			return ctx
		}).
		Teardown(func(context.Context, *testing.T, *envconf.Config) context.Context {
			tools.ResetTestBoard(t, services, model)
			return ctx
		}).
		Assess("should be reachable via ExternalName k8s service", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			zap.L().Info("Creating External service")
			reachability := matrix.NewReachability(model.AllPods(), true)
			tools.MustNoWrong(matrix.ValidateOrFail(manager, model, &matrix.TestCase{
				ToPort: 80, Protocol: v1.ProtocolTCP, Reachability: reachability, ServiceType: entities.ExternalName,
			}, false, false), t)
			return ctx
		}).Feature()

	testenv.Test(t, featureNodePortLocal, featureExternal)
}
