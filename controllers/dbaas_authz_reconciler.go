/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"reflect"
	"strings"

	"github.com/RHEcosystemAppEng/dbaas-operator/api/v1alpha1"
	oauthzv1 "github.com/openshift/api/authorization/v1"
	oauthzclientv1 "github.com/openshift/client-go/authorization/clientset/versioned/typed/authorization/v1"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type DBaaSAuthzReconciler struct {
	*DBaaSReconciler
	*oauthzclientv1.AuthorizationV1Client
}

// ResourceAccessReview for Service Admin Authz
// return users/groups who can create both inventory and secret objects in the tenant namespace
func (r *DBaaSAuthzReconciler) getServiceAdminAuthz(ctx context.Context, namespace string) v1alpha1.DBaasUsersGroups {
	logger := ctrl.LoggerFrom(ctx)
	// tenant access review
	rar := &oauthzv1.ResourceAccessReview{
		Action: oauthzv1.Action{
			Resource:  "dbaasinventories",
			Verb:      "create",
			Namespace: namespace,
			Group:     v1alpha1.GroupVersion.Group,
			Version:   v1alpha1.GroupVersion.Version,
		},
	}
	rar.SetGroupVersionKind(oauthzv1.SchemeGroupVersion.WithKind("ResourceAccessReview"))
	inventoryResponse, err := r.AuthorizationV1Client.ResourceAccessReviews().Create(ctx, rar, metav1.CreateOptions{})
	if err != nil {
		logger.Error(err, "Error creating ResourceAccessReview", inventoryResponse.Namespace, inventoryResponse.EvaluationError)
		return v1alpha1.DBaasUsersGroups{}
	}

	// secret access review
	rar.Action.Resource = "secrets"
	rar.Action.Group = corev1.SchemeGroupVersion.Group
	rar.Action.Version = corev1.SchemeGroupVersion.Version
	secretResponse, err := r.AuthorizationV1Client.ResourceAccessReviews().Create(ctx, rar, metav1.CreateOptions{})
	if err != nil {
		logger.Error(err, "Error creating ResourceAccessReview", secretResponse.Namespace, secretResponse.EvaluationError)
		return v1alpha1.DBaasUsersGroups{}
	}

	return v1alpha1.DBaasUsersGroups{
		// remove results of tenant access review, as they don't need the addtl the perms
		Users:  matchSlices(inventoryResponse.UsersSlice, secretResponse.UsersSlice),
		Groups: matchSlices(inventoryResponse.GroupsSlice, secretResponse.GroupsSlice),
	}
}

// ResourceAccessReview for Developer Authz
// return users/groups who can list inventories in the tenant namespace
func (r *DBaaSAuthzReconciler) getDeveloperAuthz(ctx context.Context, namespace string, inventoryList v1alpha1.DBaaSInventoryList) v1alpha1.DBaasUsersGroups {
	logger := ctrl.LoggerFrom(ctx)

	// inventory access review
	rar := &oauthzv1.ResourceAccessReview{
		Action: oauthzv1.Action{
			Resource:  "dbaasinventories",
			Verb:      "list",
			Namespace: namespace,
			Group:     v1alpha1.GroupVersion.Group,
			Version:   v1alpha1.GroupVersion.Version,
		},
	}
	rar.SetGroupVersionKind(oauthzv1.SchemeGroupVersion.WithKind("ResourceAccessReview"))
	inventoryResponse, err := r.AuthorizationV1Client.ResourceAccessReviews().Create(ctx, rar, metav1.CreateOptions{})
	if err != nil {
		logger.Error(err, "Error creating ResourceAccessReview", inventoryResponse.Namespace, inventoryResponse.EvaluationError)
		return v1alpha1.DBaasUsersGroups{}
	}

	return v1alpha1.DBaasUsersGroups{
		Users:  inventoryResponse.UsersSlice,
		Groups: inventoryResponse.GroupsSlice,
	}
}

// ResourceAccessReview for Service Admin Authz
// return users/groups who can create both inventory and secret objects in the tenant namespace
func (r *DBaaSAuthzReconciler) getTenantListAuthz(ctx context.Context) v1alpha1.DBaasUsersGroups {
	logger := ctrl.LoggerFrom(ctx)
	// tenant access review
	rar := &oauthzv1.ResourceAccessReview{
		Action: oauthzv1.Action{
			Resource: "dbaastenants",
			Verb:     "list",
			Group:    v1alpha1.GroupVersion.Group,
			Version:  v1alpha1.GroupVersion.Version,
		},
	}
	rar.SetGroupVersionKind(oauthzv1.SchemeGroupVersion.WithKind("ResourceAccessReview"))
	tenantResponse, err := r.AuthorizationV1Client.ResourceAccessReviews().Create(ctx, rar, metav1.CreateOptions{})
	if err != nil {
		logger.Error(err, "Error creating ResourceAccessReview", tenantResponse.Namespace, tenantResponse.EvaluationError)
		return v1alpha1.DBaasUsersGroups{}
	}

	return v1alpha1.DBaasUsersGroups{
		Users:  tenantResponse.UsersSlice,
		Groups: tenantResponse.GroupsSlice,
	}
}

// Reconcile tenant to ensure proper RBAC is created. inventoryList should only contain inventory objects for the corresponding tenant namespace.
func (r *DBaaSAuthzReconciler) reconcileTenantRbacObjs(ctx context.Context, tenant v1alpha1.DBaaSTenant, inventoryList v1alpha1.DBaaSInventoryList, serviceAdminAuthz, developerAuthz, tenantListAuthz v1alpha1.DBaasUsersGroups) error {
	devAuthz := consolidateDevAuthz(tenant, inventoryList, developerAuthz)
	clusterRole, clusterRolebinding := tenantRbacObjs(tenant, serviceAdminAuthz, devAuthz, tenantListAuthz)
	var clusterRoleObj rbacv1.ClusterRole
	if exists, err := r.createRbacObj(&clusterRole, &clusterRoleObj, &tenant, ctx); err != nil {
		return err
	} else if exists {
		if !reflect.DeepEqual(clusterRole.Rules, clusterRoleObj.Rules) {
			clusterRoleObj.Rules = clusterRole.Rules
			if err := r.updateIfOwned(ctx, &tenant, &clusterRoleObj); err != nil {
				return err
			}
		}
	}
	var clusterRoleBindingObj rbacv1.ClusterRoleBinding
	if exists, err := r.createRbacObj(&clusterRolebinding, &clusterRoleBindingObj, &tenant, ctx); err != nil {
		return err
	} else if exists {
		if !reflect.DeepEqual(clusterRolebinding.RoleRef, clusterRoleBindingObj.RoleRef) ||
			!reflect.DeepEqual(clusterRolebinding.Subjects, clusterRoleBindingObj.Subjects) {
			clusterRoleBindingObj.RoleRef = clusterRolebinding.RoleRef
			clusterRoleBindingObj.Subjects = clusterRolebinding.Subjects
			if err := r.updateIfOwned(ctx, &tenant, &clusterRoleBindingObj); err != nil {
				return err
			}
		}
	}

	return nil
}

// Reconcile inventory to ensure proper RBAC is created. tenantList should only contain tenant objects for the corresponding inventory namespace.
func (r *DBaaSAuthzReconciler) reconcileInventoryRbacObjs(ctx context.Context, inventory v1alpha1.DBaaSInventory, tenantList v1alpha1.DBaaSTenantList) error {
	role, rolebinding := inventoryRbacObjs(inventory, tenantList)
	var roleObj rbacv1.Role
	if exists, err := r.createRbacObj(&role, &roleObj, &inventory, ctx); err != nil {
		return err
	} else if exists {
		if !reflect.DeepEqual(role.Rules, roleObj.Rules) {
			roleObj.Rules = role.Rules
			if err := r.updateIfOwned(ctx, &inventory, &roleObj); err != nil {
				return err
			}
		}
	}
	var roleBindingObj rbacv1.RoleBinding
	if exists, err := r.createRbacObj(&rolebinding, &roleBindingObj, &inventory, ctx); err != nil {
		return err
	} else if exists {
		if !reflect.DeepEqual(rolebinding.RoleRef, roleBindingObj.RoleRef) ||
			!reflect.DeepEqual(rolebinding.Subjects, roleBindingObj.Subjects) {
			roleBindingObj.RoleRef = rolebinding.RoleRef
			roleBindingObj.Subjects = rolebinding.Subjects
			if err := r.updateIfOwned(ctx, &inventory, &roleBindingObj); err != nil {
				return err
			}
		}
	}

	return nil
}

// create RBAC object, return true if already exists
func (r *DBaaSAuthzReconciler) createRbacObj(newObj, getObj, owner client.Object, ctx context.Context) (exists bool, err error) {
	name := newObj.GetName()
	namespace := newObj.GetNamespace()
	kind := newObj.GetObjectKind().GroupVersionKind().Kind
	logger := ctrl.LoggerFrom(ctx)
	if hasNoEditOrListVerbs(newObj) {
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, getObj); err != nil {
			if errors.IsNotFound(err) {
				logger.V(1).Info(kind+" resource not found", name, namespace)
				if err = r.createOwnedObject(newObj, owner, ctx); err != nil {
					logger.Error(err, "Error creating resource", name, namespace)
					return false, err
				}
				logger.Info(kind+" resource created", name, namespace)
			} else {
				logger.Error(err, "Error getting the resource", name, namespace)
				return false, err
			}
		} else {
			return true, nil
		}
	} else {
		logger.V(1).Info(kind+" contains edit or list verbs, will not create", name, namespace)
	}
	return false, nil
}

func consolidateDevAuthz(tenant v1alpha1.DBaaSTenant, inventoryList v1alpha1.DBaaSInventoryList, developerAuthz v1alpha1.DBaasUsersGroups) v1alpha1.DBaasUsersGroups {
	newDevAuthz := getDevAuthzFromInventoryList(inventoryList, tenant)
	users := append(developerAuthz.Users, newDevAuthz.Users...)
	groups := append(developerAuthz.Groups, newDevAuthz.Groups...)

	return v1alpha1.DBaasUsersGroups{
		Users:  uniqueStrSlice(users),
		Groups: uniqueStrSlice(groups),
	}
}

// gets rbac objects for an inventory's users
func inventoryRbacObjs(inventory v1alpha1.DBaaSInventory, tenantList v1alpha1.DBaaSTenantList) (rbacv1.Role, rbacv1.RoleBinding) {
	role := rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dbaas-" + inventory.Name + "-inventory-viewer",
			Namespace: inventory.Namespace,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups:     []string{v1alpha1.GroupVersion.Group},
				Resources:     []string{"dbaasinventories", "dbaasinventories/status"},
				ResourceNames: []string{inventory.Name},
				Verbs:         []string{"get"},
			},
		},
	}
	role.SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind("Role"))

	roleBinding := rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      role.Name + "s",
			Namespace: role.Namespace,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.SchemeGroupVersion.Group,
			Kind:     "Role",
			Name:     role.Name,
		},
	}
	roleBinding.SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind("RoleBinding"))

	// if inventory.Spec.Authz is nil, use tenant defaults for view access to the Inventory object
	var users, groups []string
	if inventory.Spec.Authz.Users == nil && inventory.Spec.Authz.Groups == nil {
		for _, tenant := range tenantList.Items {
			if tenant.Spec.InventoryNamespace == inventory.Namespace {
				users = append(users, tenant.Spec.Authz.Users...)
				groups = append(groups, tenant.Spec.Authz.Groups...)
			}
		}
	} else {
		users = inventory.Spec.Authz.Users
		groups = inventory.Spec.Authz.Groups
	}
	users = uniqueStrSlice(users)
	groups = uniqueStrSlice(groups)

	roleBinding.Subjects = getSubjects(users, groups, role.Namespace)

	return role, roleBinding
}

// gets rbac objects for a tenant's users
func tenantRbacObjs(tenant v1alpha1.DBaaSTenant, serviceAdminAuthz, developerAuthz, tenantListAuthz v1alpha1.DBaasUsersGroups) (rbacv1.ClusterRole, rbacv1.ClusterRoleBinding) {
	clusterRole := rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dbaas-" + tenant.Name + "-tenant-viewer",
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups:     []string{v1alpha1.GroupVersion.Group},
				Resources:     []string{"dbaastenants", "dbaastenants/status"},
				ResourceNames: []string{tenant.Name},
				Verbs:         []string{"get"},
			},
		},
	}
	clusterRole.SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind("ClusterRole"))

	clusterRoleBinding := rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: clusterRole.Name + "s",
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.SchemeGroupVersion.Group,
			Kind:     "ClusterRole",
			Name:     clusterRole.Name,
		},
	}
	clusterRoleBinding.SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind("ClusterRoleBinding"))

	// give view access to all inventory devs and tenant service admins for the Tenant object
	users := uniqueStrSlice(append(serviceAdminAuthz.Users, developerAuthz.Users...))
	groups := uniqueStrSlice(append(serviceAdminAuthz.Groups, developerAuthz.Groups...))
	// remove results of tenant access review, as they don't need the addtl the perms
	users = removeFromSlice(tenantListAuthz.Users, users)
	groups = removeFromSlice(tenantListAuthz.Groups, groups)

	clusterRoleBinding.Subjects = getSubjects(users, groups, "")

	return clusterRole, clusterRoleBinding
}

// verify no edit or list permissions are assigned to a role
func hasNoEditOrListVerbs(roleObj client.Object) bool {
	var verbs []string
	var roleRules []rbacv1.PolicyRule
	editVerbs := []string{"create", "patch", "update", "delete", "list"}

	kind := roleObj.GetObjectKind().GroupVersionKind().Kind
	if kind == "Role" {
		roleRules = roleObj.(*rbacv1.Role).Rules
	}
	if kind == "ClusterRole" {
		roleRules = roleObj.(*rbacv1.ClusterRole).Rules
	}
	for _, rules := range roleRules {
		verbs = append(verbs, rules.Verbs...)
	}
	for _, verb := range editVerbs {
		if contains(verbs, verb) {
			return false
		}
	}
	return true
}

// get cumulative authz from all inventories in namespace
func getDevAuthzFromInventoryList(inventoryList v1alpha1.DBaaSInventoryList, tenant v1alpha1.DBaaSTenant) (developerAuthz v1alpha1.DBaasUsersGroups) {
	var tenantDefaults bool
	for _, inventory := range inventoryList.Items {
		if inventory.Namespace == tenant.Spec.InventoryNamespace {
			// if inventory.spec.authz is nil, apply authz from tenant.spec.authz.developer as a default
			if inventory.Spec.Authz.Users == nil && inventory.Spec.Authz.Groups == nil {
				tenantDefaults = true
			} else {
				developerAuthz.Users = append(developerAuthz.Users, inventory.Spec.Authz.Users...)
				developerAuthz.Groups = append(developerAuthz.Groups, inventory.Spec.Authz.Groups...)
			}
		}
	}
	if tenantDefaults {
		developerAuthz.Users = append(developerAuthz.Users, tenant.Spec.Authz.Users...)
		developerAuthz.Groups = append(developerAuthz.Groups, tenant.Spec.Authz.Groups...)
	}
	return developerAuthz
}

// create a slice of rbac subjects for use in role bindings
func getSubjects(users, groups []string, namespace string) []rbacv1.Subject {
	subjects := []rbacv1.Subject{}
	for _, user := range users {
		if strings.HasPrefix(user, "system:serviceaccount:") {
			sa := strings.Split(user, ":")
			if len(sa) > 3 {
				subjects = append(subjects, getSubject(sa[3], sa[2], "ServiceAccount"))
			}
		} else {
			subjects = append(subjects, getSubject(user, namespace, "User"))
		}
	}
	for _, group := range groups {
		subjects = append(subjects, getSubject(group, namespace, "Group"))
	}
	if len(subjects) == 0 {
		return nil
	}

	return subjects
}

// create an rbac subject for use in role bindings
func getSubject(name, namespace, rbacObjectKind string) rbacv1.Subject {
	subject := rbacv1.Subject{
		Kind:      rbacObjectKind,
		Name:      name,
		Namespace: namespace,
	}
	if subject.Kind != "ServiceAccount" {
		subject.APIGroup = rbacv1.SchemeGroupVersion.Group
	}
	return subject
}
