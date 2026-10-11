# Operations runbook

Procedures for running provider-clickhouse. The reasons behind them are in
the [design](design.md); section numbers below refer to it. Names in the
examples (`team-a`, `app_api`, `shared-clickhouse`) are placeholders.

## Restoring from a backup or moving to a new management cluster

A managed resource only changes or drops the ClickHouse object it created.
The claim is kept in the annotations `clickhouse.m.infrabay.io/owner-uid`
and `clickhouse.m.infrabay.io/owned-id`, and it is valid only while
`owner-uid` equals the resource's own `metadata.uid` (section 4.2). The API
server assigns a new UID to every restored or re-created object, so after a
restore every `Database`, `User` and `Role` reports `NotOwned` and writes
nothing. Take the objects over again by UUID:

1. On a `ClusterProviderConfig`, set `policy.allowAdoption: true` for the
   time of the migration. A namespaced `ProviderConfig` always allows
   adoption.
2. For each resource, read the UUID from the `NotOwned` message or from
   `status.atProvider.id`, and set it as the adopt annotation:

   ```console
   kubectl annotate user api -n team-a clickhouse.m.infrabay.io/adopt=<uuid>
   ```

   The provider adopts only when the value equals the UUID it observes, so
   an object replaced in the meantime is never adopted. It writes the claim,
   removes the annotation and emits the event `Adopted`.
3. Set `policy.allowAdoption` back to `false`.

`Grant` and `RoleGrant` have no claim of their own. They act on a grantee
that a `User` or `Role` in the same namespace owns (section 4.3), so they
work again once their grantees are adopted.

**Password users.** A restored connection Secret still carries a controller
reference to the old resource's UID. The provider never trusts or takes over
a Secret that the resource does not control (`SecretConflict`, section 5.4).
To keep the current password:

1. Put the password from the old connection Secret into a new Secret, and
   point `authentication.password.passwordSecretRef` at it.
2. Delete the restored connection Secret.
3. Adopt the user as above. The provider applies the same password and
   publishes the connection Secret again.
4. Optionally switch back to `generate`; the recorded password is kept until
   `generate.rotationToken` is set or changed.

Adopting a user that uses `generate` without these steps applies a new
password at once (Warning event `PasswordRegenerated`), and clients that use
the old one can no longer log in.

## Recovering from a lost create

A `Database` names its UUID in `CREATE DATABASE`, so a create whose
response was lost is recognised by its UUID.

For `User` and `Role` the provider records every `CREATE` it sends in
memory. When the response was lost, the next reconcile recognises the entity
and claims it (event `CreateRecovered`). After a provider restart that
record is gone, and the resource reports `NotOwned` with the hint that a
create by this resource did not complete. Check that the entity is the one
the resource created, for example in `system.query_log`, where every
statement of the provider carries `log_comment = '<Kind>/<namespace>/<name>'`,
then adopt it by UUID as above.

## Importing existing grants and memberships

Deleting a `Grant` or `RoleGrant` revokes its privileges or membership, even
if they existed before the resource did (OQ-29). To import grants that must
survive the resource, leave `Delete` out of the management policies:

```yaml
spec:
  managementPolicies: ["Observe", "Create", "Update"]
```

The grantee must be owned: adopt the user or role with a `User` or `Role`
first.

## Rotating passwords

- `generate`: set or change `generate.rotationToken`. The token that was
  applied is recorded in the connection Secret (event `PasswordRotated`).
- `passwordSecretRef`: change the value in the referenced Secret.

A connection Secret that was deleted makes the provider generate a new
password, because the server never shows the old one (Warning event
`PasswordRegenerated`).

## Unblocking a stuck deletion

A resource whose object is not its own, whose name or namespace the policy
no longer allows, or whose management policies leave out `Delete`, is
released without SQL (Warning events `Orphaned` and `OrphanedByPolicy`).
The cases that block deletion on purpose:

- **`DatabaseNotEmpty`**: the database still has tables, or the provider
  cannot verify that it has none (it needs `SHOW TABLES` on the database;
  with `spec.cluster` emptiness can never be verified, section 5.3). Drop the
  tables, set `forceDrop: true`, or remove `Delete` from the management
  policies, which leaves the database in place.
- **`PermissionDenied`**: the provider identity lacks the privilege that the
  `DROP` or `REVOKE` needs; the message names it. Grant it, or remove
  `Delete` from the management policies.

## Grant options the server does not list

ClickHouse leaves a grant option out of `system.grants` and `SHOW GRANTS`
when the privileges at its object equal those inherited from above and
another name at the same level begins with the same character, for example
`SELECT ON app.events WITH GRANT OPTION` under `SELECT ON app.*` next to a
grant on `app.events_raw` (section 5.6, A57). A `Grant` with
`withGrantOption` therefore records the grant option it gave in the
annotation `clickhouse.m.infrabay.io/grant-option`.

The provider cannot see a grant option that someone revokes where the server
hides it (OQ-31), or that a `REVOKE GRANT OPTION FOR` at a broader target
removes before the `Grant` saw the broader grant option (OQ-33): the `Grant`
then reports up to date although members of the grantee cannot delegate the
privilege. Remove the record, and the `Grant` grants the option again on its
next reconcile:

```console
kubectl annotate grant reader-events -n team-a clickhouse.m.infrabay.io/grant-option-
```

Do not edit the record otherwise: it decides what the `Grant` assumes about,
and revokes from, its own grantee at its own target.

## User directories that share a name

`system.users.storage` and `system.roles.storage` hold the name of an
entity's user directory, and directories may share a name: two
`local_directory` entries without a `<name>` are both called
`local_directory`. When a read-only directory shares the name, the provider
cannot tell its entities apart and treats every entity of that name as
read-only (`ReadOnlyStorage`). While new entities would be stored in such a
directory, every write to users, roles, grants and role grants fails with
`InvalidConfig` (section 4.2). Give each directory its own name; the server
reads `user_directories` only at start:

```xml
<clickhouse>
  <user_directories replace="replace">
    <users_xml><path>/etc/clickhouse-server/users.xml</path></users_xml>
    <local_directory><name>sql</name><path>/var/lib/clickhouse/access/</path></local_directory>
    <local_directory><name>sql_ro</name><path>/var/lib/clickhouse/access_ro/</path><readonly>true</readonly></local_directory>
  </user_directories>
</clickhouse>
```

## A provider identity without the optional settings grant

`GRANT SELECT ON system.settings_profile_elements` is needed only by users
that set `settingsProfile` (section 9.4). Without it, other users work, and
the late initialization of `settingsProfile` is skipped: after the server
refuses the read once, the provider does not try again for ten minutes per
provider config, so the server logs at most one denied query in that time.
