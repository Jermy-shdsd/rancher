package globaldns

import (
	"fmt"
	"strings"

	"github.com/rancher/rancher/pkg/namespace"
	v1coreRancher "github.com/rancher/types/apis/core/v1"
	v1beta1Rancher "github.com/rancher/types/apis/extensions/v1beta1"
	"github.com/rancher/types/apis/management.cattle.io/v3"
	"github.com/rancher/types/config"
	"k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

type UserGlobalDNSController struct {
	ingressLister         v1beta1Rancher.IngressLister
	globalDNSs            v3.GlobalDNSInterface
	multiclusterappLister v3.MultiClusterAppLister
	namespaceLister       v1coreRancher.NamespaceLister
	clusterName           string
}

func newUserGlobalDNSController(clusterContext *config.UserContext) *UserGlobalDNSController {
	g := UserGlobalDNSController{
		ingressLister:         clusterContext.Extensions.Ingresses("").Controller().Lister(),
		globalDNSs:            clusterContext.Management.Management.GlobalDNSs(""),
		multiclusterappLister: clusterContext.Management.Management.MultiClusterApps("").Controller().Lister(),
		namespaceLister:       clusterContext.Core.Namespaces("").Controller().Lister(),
		clusterName:           clusterContext.ClusterName,
	}
	return &g
}

func (g *UserGlobalDNSController) sync(key string, obj *v3.GlobalDNS) (runtime.Object, error) {
	if obj == nil || obj.DeletionTimestamp != nil {
		return nil, nil
	}

	var targetEndpoints []string
	var err error

	if obj.Spec.MultiClusterAppName != "" {
		targetEndpoints, err = g.reconcileMultiClusterApp(obj)
	} else if len(obj.Spec.ProjectNames) > 0 {
		targetEndpoints, err = g.reconcileProjects(obj)
	}

	if err != nil {
		return nil, err
	}

	//compare with the clusterEndpoints and find endpoints to update and remove.
	return g.refreshGlobalDNSEndpoints(obj, targetEndpoints)
}

func (g *UserGlobalDNSController) reconcileMultiClusterApp(obj *v3.GlobalDNS) ([]string, error) {
	// If multiclusterappID is set, look for ingresses in the projects of multiclusterapp's targets
	// Get multiclusterapp by name set on GlobalDNS spec
	mcappName, err := getMultiClusterAppName(obj.Spec.MultiClusterAppName)
	if err != nil {
		return nil, err
	}
	mcapp, err := g.multiclusterappLister.Get(namespace.GlobalNamespace, mcappName)
	if err != nil {
		return nil, err
	}

	// go through target projects which are part of the current cluster and find all ingresses
	var allIngresses []*v1beta1.Ingress

	for _, t := range mcapp.Spec.Targets {
		split := strings.SplitN(t.ProjectName, ":", 2)
		if len(split) != 2 {
			return nil, fmt.Errorf("error in splitting project ID %v", t.ProjectName)
		}
		// check if the target project in this iteration is same as the cluster in current context
		if split[0] != g.clusterName {
			continue
		}

		// each target will have appName, this appName is also the namespace in which all workloads for this app are created
		ingresses, err := g.ingressLister.List(t.AppName, labels.NewSelector())
		if err != nil {
			return nil, err
		}
		allIngresses = append(allIngresses, ingresses...)
	}

	//gather endpoints
	return g.fetchGlobalDNSEndpointsForIngresses(allIngresses, obj)
}

func (g *UserGlobalDNSController) reconcileProjects(obj *v3.GlobalDNS) ([]string, error) {
	// go through target projects which are part of the current cluster and find all ingresses
	var allIngresses []*v1beta1.Ingress

	allNamespaces, err := g.namespaceLister.List("", labels.NewSelector())
	if err != nil {
		return nil, fmt.Errorf("UserGlobalDNSController: Error listing cluster namespaces")
	}

	for _, projectNameSet := range obj.Spec.ProjectNames {
		split := strings.SplitN(projectNameSet, ":", 2)
		if len(split) != 2 {
			return nil, fmt.Errorf("UserGlobalDNSController: Error in splitting project Name %v", projectNameSet)
		}
		// check if the project in this iteration belongs to the same cluster in current context
		if split[0] != g.clusterName {
			continue
		}
		projectID := split[1]
		//list all namespaces in this project and list all ingresses within each namespace
		var namespacesInProject []string
		for _, namespace := range allNamespaces {
			nameSpaceProject := namespace.ObjectMeta.Labels[projectSelectorLabel]
			if strings.EqualFold(projectID, nameSpaceProject) {
				namespacesInProject = append(namespacesInProject, namespace.Name)
			}
		}
		for _, namespace := range namespacesInProject {
			ingresses, err := g.ingressLister.List(namespace, labels.NewSelector())
			if err != nil {
				return nil, err
			}
			allIngresses = append(allIngresses, ingresses...)
		}
	}
	//gather endpoints
	return g.fetchGlobalDNSEndpointsForIngresses(allIngresses, obj)
}

func (g *UserGlobalDNSController) fetchGlobalDNSEndpointsForIngresses(ingresses []*v1beta1.Ingress, obj *v3.GlobalDNS) ([]string, error) {
	if len(ingresses) == 0 {
		return nil, nil
	}

	var allEndpoints []string
	//gather endpoints from all ingresses
	for _, ing := range ingresses {
		if gdns, ok := ing.Annotations[annotationGlobalDNS]; ok {
			// check if the globalDNS in annotation is same as the FQDN set on the GlobalDNS
			if gdns != obj.Spec.FQDN {
				continue
			}
			//gather endpoints from the ingress
			ingressEndpoints := gatherIngressEndpoints(ing.Status.LoadBalancer.Ingress)
			allEndpoints = append(allEndpoints, ingressEndpoints...)
		}
	}

	return allEndpoints, nil
}

func (g *UserGlobalDNSController) refreshGlobalDNSEndpoints(globalDNS *v3.GlobalDNS, ingressEndpointsForCluster []string) (*v3.GlobalDNS, error) {

	globalDNSToUpdate := globalDNS.DeepCopy()

	if len(globalDNSToUpdate.Status.ClusterEndpoints) == 0 {
		globalDNSToUpdate.Status.ClusterEndpoints = make(map[string][]string)
	}

	clusterEps := globalDNSToUpdate.Status.ClusterEndpoints[g.clusterName]

	if ifEndpointsDiffer(clusterEps, ingressEndpointsForCluster) {
		globalDNSToUpdate.Status.ClusterEndpoints[g.clusterName] = ingressEndpointsForCluster
		reconcileGlobalDNSEndpoints(globalDNSToUpdate)
		updated, err := g.globalDNSs.Update(globalDNSToUpdate)
		if err != nil {
			return updated, fmt.Errorf("UserGlobalDNSController: Failed to update GlobalDNS endpoints with error %v", err)
		}
		return updated, nil
	}
	return nil, nil
}
