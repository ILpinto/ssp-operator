package common_templates

import (
	"fmt"
	"strings"

	"path/filepath"
	"sync"

	templatev1 "github.com/openshift/api/template/v1"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"kubevirt.io/ssp-operator/internal/common"
	"kubevirt.io/ssp-operator/internal/operands"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	loadTemplatesOnce sync.Once
	templatesBundle   []templatev1.Template
)

// Define RBAC rules needed by this operand:
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=template.openshift.io,resources=templates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;roles;rolebindings,verbs=get;list;watch;create;update;patch;delete

// RBAC for created roles
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datavolumes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datavolumes/source,verbs=create

type commonTemplates struct{}

var _ operands.Operand = &commonTemplates{}

func GetOperand() operands.Operand {
	return &commonTemplates{}
}

func (c *commonTemplates) Name() string {
	return operandName
}

const (
	operandName      = "common-templates"
	operandComponent = common.AppComponentTemplating
)

func (c *commonTemplates) AddWatchTypesToScheme(s *runtime.Scheme) error {
	return templatev1.Install(s)
}

func (c *commonTemplates) WatchClusterTypes() []client.Object {
	return []client.Object{
		&rbac.ClusterRole{},
		&rbac.Role{},
		&rbac.RoleBinding{},
		&core.Namespace{},
		&templatev1.Template{},
	}
}

func (c *commonTemplates) WatchTypes() []client.Object {
	return nil
}

func (c *commonTemplates) Reconcile(request *common.Request) ([]common.ResourceStatus, error) {
	funcs := []common.ReconcileFunc{
		reconcileGoldenImagesNS,
		reconcileViewRole,
		reconcileViewRoleBinding,
		reconcileEditRole,
	}

	oldTemplateFuncs, err := reconcileOlderTemplates(request)
	if err != nil {
		return nil, err
	}

	funcs = append(funcs, oldTemplateFuncs...)
	funcs = append(funcs, reconcileTemplatesFuncs(request)...)

	return common.CollectResourceStatus(request, funcs...)
}

func (c *commonTemplates) Cleanup(request *common.Request) error {
	objects := []client.Object{
		newGoldenImagesNS(GoldenImagesNSname),
		newViewRole(GoldenImagesNSname),
		newViewRoleBinding(GoldenImagesNSname),
		newEditRole(),
	}
	namespace := request.Instance.Spec.CommonTemplates.Namespace
	for index := range templatesBundle {
		templatesBundle[index].ObjectMeta.Namespace = namespace
		objects = append(objects, &templatesBundle[index])
	}
	for _, obj := range objects {
		err := request.Client.Delete(request.Context, obj)
		if err != nil && !errors.IsNotFound(err) {
			request.Logger.Error(err, fmt.Sprintf("Error deleting \"%s\": %s", obj.GetName(), err))
			return err
		}
	}
	return nil
}

func reconcileGoldenImagesNS(request *common.Request) (common.ResourceStatus, error) {
	return common.CreateOrUpdate(request).
		ClusterResource(newGoldenImagesNS(GoldenImagesNSname)).
		WithAppLabels(operandName, operandComponent).
		Reconcile()
}

func reconcileViewRole(request *common.Request) (common.ResourceStatus, error) {
	return common.CreateOrUpdate(request).
		ClusterResource(newViewRole(GoldenImagesNSname)).
		WithAppLabels(operandName, operandComponent).
		UpdateFunc(func(newRes, foundRes client.Object) {
			foundRole := foundRes.(*rbac.Role)
			newRole := newRes.(*rbac.Role)
			foundRole.Rules = newRole.Rules
		}).
		Reconcile()
}

func reconcileViewRoleBinding(request *common.Request) (common.ResourceStatus, error) {
	return common.CreateOrUpdate(request).
		ClusterResource(newViewRoleBinding(GoldenImagesNSname)).
		WithAppLabels(operandName, operandComponent).
		UpdateFunc(func(newRes, foundRes client.Object) {
			newBinding := newRes.(*rbac.RoleBinding)
			foundBinding := foundRes.(*rbac.RoleBinding)
			foundBinding.Subjects = newBinding.Subjects
			foundBinding.RoleRef = newBinding.RoleRef
		}).
		Reconcile()
}

func reconcileEditRole(request *common.Request) (common.ResourceStatus, error) {
	return common.CreateOrUpdate(request).
		ClusterResource(newEditRole()).
		WithAppLabels(operandName, operandComponent).
		UpdateFunc(func(newRes, foundRes client.Object) {
			newRole := newRes.(*rbac.ClusterRole)
			foundRole := foundRes.(*rbac.ClusterRole)
			foundRole.Rules = newRole.Rules
		}).
		Reconcile()
}

func reconcileOlderTemplates(request *common.Request) ([]common.ReconcileFunc, error) {
	// Append functions to take ownership of previously deployed templates during an upgrade
	templatesSelector := func() labels.Selector {
		baseRequirement, err := labels.NewRequirement(TemplateTypeLabel, selection.Equals, []string{"base"})
		if err != nil {
			panic(fmt.Sprintf("Failed creating label selector for '%s=%s'", TemplateTypeLabel, "base"))
		}

		// Only fetching older templates  to prevent duplication of API calls
		versionRequirement, err := labels.NewRequirement(TemplateVersionLabel, selection.NotEquals, []string{Version})
		if err != nil {
			panic(fmt.Sprintf("Failed creating label selector for '%s!=%s'", TemplateVersionLabel, Version))
		}

		return labels.NewSelector().Add(*baseRequirement, *versionRequirement)
	}()

	existingTemplates := &templatev1.TemplateList{}
	err := request.Client.List(request.Context, existingTemplates, &client.ListOptions{
		LabelSelector: templatesSelector,
		Namespace:     request.Instance.Spec.CommonTemplates.Namespace,
	})

	// There might not be any templates (in case of a fresh deployment), so a NotFound error is accepted
	if err != nil && !errors.IsNotFound(err) {
		return nil, err
	}

	funcs := make([]common.ReconcileFunc, 0, len(existingTemplates.Items))
	for i := range existingTemplates.Items {
		template := &existingTemplates.Items[i]
		if template.Annotations == nil {
			template.Annotations = make(map[string]string)
		}
		template.Annotations[TemplateDeprecatedAnnotation] = "true"
		funcs = append(funcs, func(*common.Request) (common.ResourceStatus, error) {
			return common.CreateOrUpdate(request).
				ClusterResource(template).
				WithAppLabels(operandName, operandComponent).
				UpdateFunc(func(_, foundRes client.Object) {
					foundTemplate := foundRes.(*templatev1.Template)
					for key := range foundTemplate.Labels {
						if strings.HasPrefix(key, TemplateOsLabelPrefix) ||
							strings.HasPrefix(key, TemplateFlavorLabelPrefix) ||
							strings.HasPrefix(key, TemplateWorkloadLabelPrefix) {
							delete(foundTemplate.Labels, key)
						}
					}
				}).
				Reconcile()
		})
	}

	return funcs, nil
}

func reconcileTemplatesFuncs(request *common.Request) []common.ReconcileFunc {
	loadTemplates := func() {
		var err error
		filename := filepath.Join(BundleDir, "common-templates-"+Version+".yaml")
		templatesBundle, err = ReadTemplates(filename)
		if err != nil {
			request.Logger.Error(err, fmt.Sprintf("Error reading from template bundle, %v", err))
			panic(err)
		}
		if len(templatesBundle) == 0 {
			panic("No templates could be found in the installed bundle")
		}
	}
	// Only load templates Once
	loadTemplatesOnce.Do(loadTemplates)

	namespace := request.Instance.Spec.CommonTemplates.Namespace
	funcs := make([]common.ReconcileFunc, 0, len(templatesBundle))
	for i := range templatesBundle {
		template := &templatesBundle[i]
		template.ObjectMeta.Namespace = namespace
		funcs = append(funcs, func(request *common.Request) (common.ResourceStatus, error) {
			return common.CreateOrUpdate(request).
				ClusterResource(template).
				WithAppLabels(operandName, operandComponent).
				UpdateFunc(func(newRes, foundRes client.Object) {
					newTemplate := newRes.(*templatev1.Template)
					foundTemplate := foundRes.(*templatev1.Template)
					foundTemplate.Objects = newTemplate.Objects
					foundTemplate.Parameters = newTemplate.Parameters
				}).
				Reconcile()
		})
	}
	return funcs
}
