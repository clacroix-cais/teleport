/*
Copyright 2021 Gravitational, Inc.

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

package auth

import (
	"context"

	"github.com/gravitational/trace"

	"github.com/gravitational/teleport/api/types"
	apievents "github.com/gravitational/teleport/api/types/events"
	"github.com/gravitational/teleport/lib/events"
)

// UpsertRole creates or updates a role and emits a related audit event.
func (a *Server) UpsertRole(ctx context.Context, role types.Role) error {
	if err := a.checkRoleRulesConstraint(ctx, role, "update"); err != nil {
		return trace.Wrap(err)
	}

	if err := a.Access.UpsertRole(ctx, role); err != nil {
		return trace.Wrap(err)
	}

	if err := a.emitter.EmitAuditEvent(a.closeCtx, &apievents.RoleCreate{
		Metadata: apievents.Metadata{
			Type: events.RoleCreatedEvent,
			Code: events.RoleCreatedCode,
		},
		UserMetadata: ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name: role.GetName(),
		},
	}); err != nil {
		log.WithError(err).Warnf("Failed to emit role create event.")
	}
	return nil
}

// DeleteRole deletes a role and emits a related audit event.
func (a *Server) DeleteRole(ctx context.Context, name string) error {
	// check if this role is used by CA or Users
	users, err := a.Services.GetUsers(false)
	if err != nil {
		return trace.Wrap(err)
	}
	for _, u := range users {
		for _, r := range u.GetRoles() {
			if r == name {
				// Mask the actual error here as it could be used to enumerate users
				// within the system.
				log.Warnf("Failed to delete role: role %q is used by user %q.", name, u.GetName())
				return trace.BadParameter("failed to delete role that still in use by a user. Check system server logs for more details.")
			}
		}
	}
	// check if it's used by some external cert authorities, e.g.
	// cert authorities related to external cluster
	cas, err := a.Services.GetCertAuthorities(ctx, types.UserCA, false)
	if err != nil {
		return trace.Wrap(err)
	}
	for _, a := range cas {
		for _, r := range a.GetRoles() {
			if r == name {
				// Mask the actual error here as it could be used to enumerate users
				// within the system.
				log.Warnf("Failed to delete role: role %q is used by user cert authority %q", name, a.GetClusterName())
				return trace.BadParameter("failed to delete role that still in use by a user. Check system server logs for more details.")
			}
		}
	}

	if err := a.Access.DeleteRole(ctx, name); err != nil {
		return trace.Wrap(err)
	}

	if err := a.emitter.EmitAuditEvent(a.closeCtx, &apievents.RoleDelete{
		Metadata: apievents.Metadata{
			Type: events.RoleDeletedEvent,
			Code: events.RoleDeletedCode,
		},
		UserMetadata: ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name: name,
		},
	}); err != nil {
		log.WithError(err).Warnf("Failed to emit role delete event.")
	}
	return nil
}

// UpsertLock upserts a lock and emits a related audit event.
func (a *Server) UpsertLock(ctx context.Context, lock types.Lock) error {
	if err := a.Services.UpsertLock(ctx, lock); err != nil {
		return trace.Wrap(err)
	}

	um := ClientUserMetadata(ctx)
	if err := a.emitter.EmitAuditEvent(a.closeCtx, &apievents.LockCreate{
		Metadata: apievents.Metadata{
			Type: events.LockCreatedEvent,
			Code: events.LockCreatedCode,
		},
		UserMetadata: um,
		ResourceMetadata: apievents.ResourceMetadata{
			Name:      lock.GetName(),
			UpdatedBy: um.User,
		},
		Target: lock.Target(),
	}); err != nil {
		log.WithError(err).Warning("Failed to emit lock create event.")
	}
	return nil
}

// DeleteLock deletes a lock and emits a related audit event.
func (a *Server) DeleteLock(ctx context.Context, lockName string) error {
	if err := a.Services.DeleteLock(ctx, lockName); err != nil {
		return trace.Wrap(err)
	}

	if err := a.emitter.EmitAuditEvent(a.closeCtx, &apievents.LockDelete{
		Metadata: apievents.Metadata{
			Type: events.LockDeletedEvent,
			Code: events.LockDeletedCode,
		},
		UserMetadata: ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name: lockName,
		},
	}); err != nil {
		log.WithError(err).Warning("Failed to emit lock delete event.")
	}
	return nil
}

// checkRoleRulesConstraint checks if the request will result in having
// no roles with rules to upsert roles.
func (s *Server) checkRoleRulesConstraint(ctx context.Context, targetRole types.Role, request string) error {
	rolesWithUpdateRolesRule, err := s.getRolesWithUpdateRolesRule(ctx)
	if err != nil {
		return trace.Wrap(err)
	}

	isRoleLosingUpdateRolesRule := Contains(rolesWithUpdateRolesRule, targetRole.GetName()) && !roleHasUpdateRolesRule(targetRole)

	if !isRoleLosingUpdateRolesRule {
		return nil
	}

	allUsers, err := s.Identity.GetUsers(false /* withSecrets */)
	if err != nil {
		return trace.Wrap(err)
	}

	localUsersWithPermissionToEditRoles := Filter(allUsers, func(u types.User) bool {
		isLocalUser := u.GetCreatedBy().Connector == nil

		rolesWithoutTarget := Filter(u.GetRoles(), func(role string) bool {
			return role != targetRole.GetName()
		})

		return isLocalUser && Some(rolesWithoutTarget, func(role string) bool {
			return Contains(rolesWithUpdateRolesRule, role)
		})
	})

	if len(localUsersWithPermissionToEditRoles) > 0 {
		return nil
	}

	log.Warnf("Failed to %s role. This operation would cause no local user to be able to edit roles.", request)
	return trace.BadParameter("Failed to %s role. This operation would cause no local user to be able to edit roles.", request)
}

// returns a list of roles that have a update rule associated to the role resource.
func (a *Server) getRolesWithUpdateRolesRule(ctx context.Context) ([]string, error) {
	allRoles, err := a.Access.GetRoles(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	getRolesWithUpdateRolesRule := Filter(allRoles, roleHasUpdateRolesRule)

	return Map(getRolesWithUpdateRolesRule, func(r types.Role) string {
		return r.GetName()
	}), nil
}

// checks if role has permission to edit roles
func roleHasUpdateRolesRule(role types.Role) bool {
	return Some(role.GetRules(types.Allow), func(rule types.Rule) bool {
		if !rule.HasResource(types.KindRole) {
			return false
		}

		if !rule.HasVerb(types.VerbUpdate) {
			return false
		}

		return true
	})
}
