package resource

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Masterminds/squirrel"
	"github.com/araddon/dateparse"
	"github.com/artpar/api2go"
	"github.com/artpar/go.uuid"
	"github.com/daptin/daptin/server/auth"
	"github.com/daptin/daptin/server/columntypes"
	"github.com/daptin/daptin/server/statementbuilder"
	"github.com/jmoiron/sqlx"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const DATE_LAYOUT = "2006-01-02 15:04:05"

// Check if a user identified by userReferenceId and belonging to userGroups is allowed to invoke an action `actionName` on type `typeName`
// Called before invoking an action from the /action/** api
// Checks EXECUTE on both the type and action for this user
// The permissions can come from different groups
func (dr *DbResource) IsUserActionAllowed(userReferenceId string, userGroups []auth.GroupPermission, typeName string, actionName string) bool {

	permission := dr.GetObjectPermissionByWhereClause("world", "table_name", typeName)

	actionPermission := dr.GetObjectPermissionByWhereClause("action", "action_name", actionName)

	canExecuteOnType := permission.CanExecute(userReferenceId, userGroups)
	canExecuteAction := actionPermission.CanExecute(userReferenceId, userGroups)

	return canExecuteOnType && canExecuteAction

}

// Get an Action instance by `typeName` and `actionName`
// Check Action instance for usage
func (dr *DbResource) GetActionByName(typeName string, actionName string) (Action, error) {
	var a ActionRow

	var action Action

	sql, args, err := statementbuilder.Squirrel.Select("a.action_name as name", "w.table_name as ontype",
		"a.label", "action_schema as action_schema", "a.reference_id as referenceid").From("action a").
		Join("world w on w.id = a.world_id").Where("w.table_name = ?", typeName).Where("a.action_name = ?", actionName).Limit(1).ToSql()

	if err != nil {
		return action, err
	}

	err = dr.db.QueryRowx(sql, args...).StructScan(&a)
	if err != nil {
		log.Errorf("Failed to scan action: %v", err)
		return action, err
	}

	err = json.Unmarshal([]byte(a.ActionSchema), &action)
	CheckErr(err, "failed to unmarshal infields")

	action.Name = a.Name
	action.Label = a.Name
	action.ReferenceId = a.ReferenceId
	action.OnType = a.OnType

	return action, nil
}

// Get list of all actions defined on type `typeName`
// Returns list of `Action`
func (dr *DbResource) GetActionsByType(typeName string) ([]Action, error) {
	action := make([]Action, 0)

	sql, args, err := statementbuilder.Squirrel.Select("a.action_name as name",
		"w.table_name as ontype", "a.label", "action_schema as action_schema", "a.instance_optional as instance_optional",
		"a.reference_id as referenceid").From("action a").Join("world w on w.id = a.world_id").Where(squirrel.Eq{
		"w.table_name": typeName,
	}).ToSql()
	if err != nil {
		return nil, err
	}

	rows, err := dr.db.Queryx(sql, args...)
	if err != nil {
		log.Errorf("Failed to scan action: %v", err)
		return action, err
	}
	defer rows.Close()

	for rows.Next() {

		var act Action
		var a ActionRow
		err := rows.StructScan(&a)
		CheckErr(err, "Failed to struct scan action row")

		if len(a.Label) < 1 {
			continue
		}
		err = json.Unmarshal([]byte(a.ActionSchema), &act)
		CheckErr(err, "failed to unmarshal infields")

		act.Name = a.Name
		act.Label = a.Label
		act.ReferenceId = a.ReferenceId
		act.OnType = a.OnType
		act.InstanceOptional = a.InstanceOptional

		action = append(action, act)

	}

	return action, nil
}

// Get permission of an action by typeId and actionName
// Loads the owner, usergroup and guest permission of the action from the database
// Return a PermissionInstance
// Special utility function for actions, for other objects use GetObjectPermissionByReferenceId
func (dr *DbResource) GetActionPermissionByName(worldId int64, actionName string) (PermissionInstance, error) {

	refId, err := dr.GetReferenceIdByWhereClause("action", squirrel.Eq{"action_name": actionName}, squirrel.Eq{"world_id": worldId})
	if err != nil {
		return PermissionInstance{}, err
	}

	if refId == nil || len(refId) < 1 {
		return PermissionInstance{}, errors.New(fmt.Sprintf("Failed to find action [%v] on [%v]", actionName, worldId))
	}
	permissions := dr.GetObjectPermissionByReferenceId("action", refId[0])

	return permissions, nil
}

// Get permission of an GetObjectPermissionByReferenceId by typeName and string referenceId
// Loads the owner, usergroup and guest permission of the action from the database
// Return a PermissionInstance
// Return a NoPermissionToAnyone if no such object exist
func (dr *DbResource) GetObjectPermissionByReferenceId(objectType string, referenceId string) PermissionInstance {

	var selectQuery string
	var queryParameters []interface{}
	var err error
	if objectType == "usergroup" {
		selectQuery, queryParameters, err = statementbuilder.Squirrel.
			Select("permission", "id").
			From(objectType).Where(squirrel.Eq{"reference_id": referenceId}).
			ToSql()
	} else {
		selectQuery, queryParameters, err = statementbuilder.Squirrel.
			Select(USER_ACCOUNT_ID_COLUMN, "permission", "id").
			From(objectType).Where(squirrel.Eq{"reference_id": referenceId}).
			ToSql()

	}

	if err != nil {
		log.Errorf("Failed to create sql: %v", err)
		return PermissionInstance{
			"", []auth.GroupPermission{}, auth.AuthPermission(0),
		}
	}

	resultObject := make(map[string]interface{})
	err = dr.db.QueryRowx(selectQuery, queryParameters...).MapScan(resultObject)
	if err != nil {
		log.Errorf("Failed to scan permission 1 [%v]: %v", referenceId, err)
	}
	//log.Infof("permi map: %v", resultObject)
	var perm PermissionInstance
	if resultObject[USER_ACCOUNT_ID_COLUMN] != nil {

		user, err := dr.GetIdToReferenceId(USER_ACCOUNT_TABLE_NAME, resultObject[USER_ACCOUNT_ID_COLUMN].(int64))
		if err == nil {
			perm.UserId = user
		}

	}

	i, ok := resultObject["id"].(int64)
	if !ok {
		return perm
	}
	perm.UserGroupId = dr.GetObjectGroupsByObjectId(objectType, i)

	perm.Permission = auth.AuthPermission(resultObject["permission"].(int64))
	if err != nil {
		log.Errorf("Failed to scan permission 2: %v", err)
	}

	//log.Infof("PermissionInstance for [%v]: %v", typeName, perm)
	return perm
}

// Get permission of an GetObjectPermissionById by typeName and string referenceId
// Loads the owner, usergroup and guest permission of the action from the database
// Return a PermissionInstance
// Return a NoPermissionToAnyone if no such object exist
func (dr *DbResource) GetObjectPermissionById(objectType string, id int64) PermissionInstance {

	var selectQuery string
	var queryParameters []interface{}
	var err error
	if objectType == "usergroup" {
		selectQuery, queryParameters, err = statementbuilder.Squirrel.
			Select("permission", "id").
			From(objectType).Where(squirrel.Eq{"id": id}).
			ToSql()
	} else {
		selectQuery, queryParameters, err = statementbuilder.Squirrel.
			Select(USER_ACCOUNT_ID_COLUMN, "permission", "id").
			From(objectType).Where(squirrel.Eq{"id": id}).
			ToSql()

	}

	if err != nil {
		log.Errorf("Failed to create sql: %v", err)
		return PermissionInstance{
			"", []auth.GroupPermission{}, auth.AuthPermission(0),
		}
	}

	resultObject := make(map[string]interface{})
	err = dr.db.QueryRowx(selectQuery, queryParameters...).MapScan(resultObject)
	if err != nil {
		log.Errorf("Failed to scan permission 3 [%v]: %v", id, err)
	}
	//log.Infof("permi map: %v", resultObject)
	var perm PermissionInstance
	if resultObject[USER_ACCOUNT_ID_COLUMN] != nil {

		user, err := dr.GetIdToReferenceId(USER_ACCOUNT_TABLE_NAME, resultObject["user_account_id"].(int64))
		if err == nil {
			perm.UserId = user
		}
	}

	perm.UserGroupId = dr.GetObjectGroupsByObjectId(objectType, resultObject["id"].(int64))

	perm.Permission = auth.AuthPermission(resultObject["permission"].(int64))
	if err != nil {
		log.Errorf("Failed to scan permission 2: %v", err)
	}

	//log.Infof("PermissionInstance for [%v]: %v", typeName, perm)
	return perm
}

// Get permission of an GetObjectPermissionByReferenceId by typeName and string referenceId with a simple where clause colName = colValue
// Use carefully
// Loads the owner, usergroup and guest permission of the action from the database
// Return a PermissionInstance
// Return a NoPermissionToAnyone if no such object exist
func (dr *DbResource) GetObjectPermissionByWhereClause(objectType string, colName string, colValue string) PermissionInstance {
	var perm PermissionInstance
	s, q, err := statementbuilder.Squirrel.Select(USER_ACCOUNT_ID_COLUMN, "permission", "id").From(objectType).Where(squirrel.Eq{colName: colValue}).ToSql()
	if err != nil {
		log.Errorf("Failed to create sql: %v", err)
		return perm
	}

	m := make(map[string]interface{})
	err = dr.db.QueryRowx(s, q...).MapScan(m)

	if err != nil {

		log.Errorf("Failed to scan permission: %v", err)
		return perm
	}

	//log.Infof("permi map: %v", m)
	if m["user_account_id"] != nil {

		user, err := dr.GetIdToReferenceId(USER_ACCOUNT_TABLE_NAME, m[USER_ACCOUNT_ID_COLUMN].(int64))
		if err == nil {
			perm.UserId = user
		}

	}

	perm.UserGroupId = dr.GetObjectGroupsByObjectId(objectType, m["id"].(int64))

	perm.Permission = auth.AuthPermission(m["permission"].(int64))

	//log.Infof("PermissionInstance for [%v]: %v", typeName, perm)
	return perm
}

// Get list of group permissions for objects of typeName where colName=colValue
// Utility method which makes a join query to load a lot of permissions quickly
// Used by GetRowPermission
func (dr *DbResource) GetObjectUserGroupsByWhere(objType string, colName string, colvalue interface{}) []auth.GroupPermission {

	s := make([]auth.GroupPermission, 0)

	rel := api2go.TableRelation{}
	rel.Subject = objType
	rel.SubjectName = objType + "_id"
	rel.Object = "usergroup"
	rel.ObjectName = "usergroup_id"
	rel.Relation = "has_many_and_belongs_to_many"

	//log.Infof("Join string: %v: ", rel.GetJoinString())

	sql, args, err := statementbuilder.Squirrel.Select("usergroup_id.reference_id as \"groupreferenceid\"",
		rel.GetJoinTableName()+".reference_id as \"relationreferenceid\"", rel.GetJoinTableName()+".permission").From(rel.Subject).Join(rel.GetJoinString()).
		Where(fmt.Sprintf("%s.%s = ?", rel.Subject, colName), colvalue).ToSql()
	if err != nil {
		log.Errorf("Failed to create permission select query: %v", err)
		return s
	}

	res, err := dr.db.Queryx(sql, args...)
	//log.Infof("Group select sql: %v", sql)
	if err != nil {

		log.Errorf("Failed to get object groups by where clause: %v", err)
		log.Errorf("Query: %s == [%v]", sql, args)
		return s
	}
	defer res.Close()

	for res.Next() {
		var g auth.GroupPermission
		err = res.StructScan(&g)
		if err != nil {
			log.Errorf("Failed to scan group permission 1: %v", err)
		}
		s = append(s, g)
	}
	return s

}
func (dr *DbResource) GetObjectGroupsByObjectId(objType string, objectId int64) []auth.GroupPermission {
	s := make([]auth.GroupPermission, 0)

	refId, err := dr.GetIdToReferenceId(objType, objectId)

	if objType == "usergroup" {

		if err != nil {
			log.Infof("Failed to get id to reference id [%v][%v] == %v", objType, objectId, err)
			return s
		}
		s = append(s, auth.GroupPermission{
			GroupReferenceId:    refId,
			ObjectReferenceId:   refId,
			RelationReferenceId: refId,
			Permission:          auth.AuthPermission(dr.Cruds["usergroup"].model.GetDefaultPermission()),
		})
		return s
	}

	sql, args, err := statementbuilder.Squirrel.Select("ug.reference_id as \"groupreferenceid\"",
		"uug.reference_id as relationreferenceid", "uug.permission").From("usergroup ug").
		Join(fmt.Sprintf("%s_%s_id_has_usergroup_usergroup_id uug on uug.usergroup_id = ug.id", objType, objType)).
		Where(fmt.Sprintf("uug.%s_id = ?", objType), objectId).ToSql()

	res, err := dr.db.Queryx(sql, args...)

	if err != nil {
		log.Errorf("Failed to query object group by object id [%v][%v] == %v", objType, objectId, err)
		return s
	}
	defer res.Close()

	for res.Next() {
		var g auth.GroupPermission
		err = res.StructScan(&g)
		g.ObjectReferenceId = refId
		if err != nil {
			log.Errorf("Failed to scan group permission 2: %v", err)
		}
		s = append(s, g)
	}
	return s

}

// Check if someone can invoke the become admin action
// checks if there is only 1 real user in the system
// No one can become admin once we have an adminstrator
func (dbResource *DbResource) CanBecomeAdmin() bool {

	adminRefId := dbResource.GetAdminReferenceId()
	if adminRefId == "" {
		return true
	}

	return false

}

// Returns the user account row of a user by looking up on email
func (d *DbResource) GetUserAccountRowByEmail(email string) (map[string]interface{}, error) {

	user, _, err := d.Cruds[USER_ACCOUNT_TABLE_NAME].GetRowsByWhereClause("user_account", squirrel.Eq{"email": email})

	if len(user) > 0 {

		return user[0], err
	}

	return nil, errors.New("no such user")

}

// Returns the user account row of a user by looking up on email
func (d *DbResource) GetUserMailAccountRowByEmail(username string) (map[string]interface{}, error) {

	mailAccount, _, err := d.Cruds["mail_account"].GetRowsByWhereClause("mail_account",
		squirrel.Eq{"username": username})

	if len(mailAccount) > 0 {

		return mailAccount[0], err
	}

	return nil, errors.New("no such mail account")

}

// Returns the user mail account box row of a user
func (d *DbResource) GetMailAccountBox(mailAccountId int64, mailBoxName string) (map[string]interface{}, error) {

	mailAccount, _, err := d.Cruds["mail_box"].GetRowsByWhereClause("mail_box", squirrel.Eq{"mail_account_id": mailAccountId}, squirrel.Eq{"name": mailBoxName})

	if len(mailAccount) > 0 {

		return mailAccount[0], err
	}

	return nil, errors.New("no such mail box")

}

// Returns the user mail account box row of a user
func (d *DbResource) CreateMailAccountBox(mailAccountId string, sessionUser *auth.SessionUser, mailBoxName string) (map[string]interface{}, error) {

	httpRequest := &http.Request{
		Method: "POST",
	}

	httpRequest = httpRequest.WithContext(context.WithValue(context.Background(), "user", sessionUser))
	resp, err := d.Cruds["mail_box"].Create(&api2go.Api2GoModel{
		Data: map[string]interface{}{
			"name":            mailBoxName,
			"mail_account_id": mailAccountId,
			"uidvalidity":     time.Now().Unix(),
			"nextuid":         1,
			"subscribed":      true,
			"attributes":      "",
			"flags":           "\\*",
			"permanent_flags": "\\*",
		},
	}, api2go.Request{
		PlainRequest: httpRequest,
	})

	return resp.Result().(*api2go.Api2GoModel).Data, err

}

// Returns the user mail account box row of a user
func (d *DbResource) DeleteMailAccountBox(mailAccountId int64, mailBoxName string) error {

	box, err := d.Cruds["mail_box"].GetAllObjectsWithWhere("mail_box",
		squirrel.Eq{
			"mail_account_id": mailAccountId,
			"name":            mailBoxName,
		},
	)
	if err != nil || len(box) == 0 {
		return errors.New("mailbox does not exist")
	}

	query, args, err := statementbuilder.Squirrel.Delete("mail").Where(squirrel.Eq{"mail_box_id": box[0]["id"]}).ToSql()
	if err != nil {
		return err
	}

	_, err = d.db.Exec(query, args...)
	if err != nil {
		return err
	}

	query, args, err = statementbuilder.Squirrel.Delete("mail_box").Where(squirrel.Eq{"id": box[0]["id"]}).ToSql()
	if err != nil {
		return err
	}

	_, err = d.db.Exec(query, args...)

	return err

}

// Returns the user mail account box row of a user
func (d *DbResource) RenameMailAccountBox(mailAccountId int64, oldBoxName string, newBoxName string) error {

	box, err := d.Cruds["mail_box"].GetAllObjectsWithWhere("mail_box",
		squirrel.Eq{
			"mail_account_id": mailAccountId,
			"name":            oldBoxName,
		},
	)
	if err != nil || len(box) == 0 {
		return errors.New("mailbox does not exist")
	}

	query, args, err := statementbuilder.Squirrel.Update("mail_box").Set("name", newBoxName).Where(squirrel.Eq{"id": box[0]["id"]}).ToSql()
	if err != nil {
		return err
	}

	_, err = d.db.Exec(query, args...)

	return err

}

// Returns the user mail account box row of a user
func (d *DbResource) SetMailBoxSubscribed(mailAccountId int64, mailBoxName string, subscribed bool) error {

	query, args, err := statementbuilder.Squirrel.Update("mail_box").Set("subscribed", subscribed).Where(squirrel.Eq{
		"mail_account_id": mailAccountId,
		"name":            mailBoxName,
	}).ToSql()
	if err != nil {
		return err
	}

	_, err = d.db.Exec(query, args...)

	return err

}

func (d *DbResource) GetUserPassword(email string) (string, error) {
	passwordHash := ""

	existingUsers, _, err := d.Cruds[USER_ACCOUNT_TABLE_NAME].GetRowsByWhereClause("user_account", squirrel.Eq{"email": email})
	if err != nil {
		return passwordHash, err
	}
	if len(existingUsers) < 1 {
		return passwordHash, errors.New("User not found")
	}

	passwordHash = existingUsers[0]["password"].(string)

	return passwordHash, err
}

// Convert group name to the internal integer id
// should not be used since group names are not unique
// deprecated
func (dbResource *DbResource) UserGroupNameToId(groupName string) (uint64, error) {

	query, arg, err := statementbuilder.Squirrel.Select("id").From("usergroup").Where(squirrel.Eq{"name": groupName}).ToSql()
	if err != nil {
		return 0, err
	}
	res := dbResource.db.QueryRowx(query, arg...)
	if res.Err() != nil {
		return 0, res.Err()
	}

	var id uint64
	err = res.Scan(&id)

	return id, err
}

// make user by integer `userId` int the administrator and owner of everything
// Check CanBecomeAdmin before invoking this
func (dbResource *DbResource) BecomeAdmin(userId int64) bool {
	log.Printf("User: %d is going to become admin", userId)
	if !dbResource.CanBecomeAdmin() {
		return false
	}

	for _, crud := range dbResource.Cruds {

		if crud.model.GetName() == "user_account_user_account_id_has_usergroup_usergroup_id" {
			continue
		}

		if crud.model.HasColumn(USER_ACCOUNT_ID_COLUMN) {
			q, v, err := statementbuilder.Squirrel.Update(crud.model.GetName()).
				Set(USER_ACCOUNT_ID_COLUMN, userId).
				Set("permission", auth.DEFAULT_PERMISSION).
				ToSql()
			if err != nil {
				log.Errorf("Query: %v", q)
				log.Errorf("Failed to create query to update to become admin: %v == %v", crud.model.GetName(), err)
				continue
			}

			_, err = dbResource.db.Exec(q, v...)
			if err != nil {
				log.Errorf("Query: %v", q)
				log.Errorf("	Failed to execute become admin update query: %v", err)
				continue
			}

		}
	}

	adminUsergroupId, err := dbResource.UserGroupNameToId("administrators")
	reference_id, err := uuid.NewV4()

	query, args, err := statementbuilder.Squirrel.Insert("user_account_user_account_id_has_usergroup_usergroup_id").
		Columns(USER_ACCOUNT_ID_COLUMN, "usergroup_id", "permission", "reference_id").
		Values(userId, adminUsergroupId, int64(auth.DEFAULT_PERMISSION), reference_id.String()).
		ToSql()

	_, err = dbResource.db.Exec(query, args...)
	CheckErr(err, "Failed to add user to administrator usergroup: %v == %v", query, args)

	_, err = dbResource.db.Exec("update world set permission = ?, default_permission = ? where table_name not like '%_audit'",
		auth.DEFAULT_PERMISSION, auth.DEFAULT_PERMISSION)
	if err != nil {
		log.Errorf("Failed to update world permissions: %v", err)
	}

	_, err = dbResource.db.Exec("update world set permission = ?, default_permission = ? where table_name like '%_audit'",
		int64(auth.GuestCreate|auth.UserCreate|auth.GroupCreate),
		int64(auth.GuestRead|auth.UserRead|auth.GroupRead))
	if err != nil {
		log.Errorf("Failed to world update audit permissions: %v", err)
	}

	_, err = dbResource.db.Exec("update action set permission = ?", int64(auth.UserRead|auth.UserExecute|auth.GroupCRUD|auth.GroupExecute|auth.GroupRefer))
	_, err = dbResource.db.Exec("update action set permission = ? where action_name in ('signin')", int64(auth.GuestPeek|auth.GuestExecute|auth.UserRead|auth.UserExecute|auth.GroupRead|auth.GroupExecute))

	if err != nil {
		log.Errorf("Failed to update audit permissions: %v", err)
	}

	return true
}

func (dr *DbResource) GetRowPermission(row map[string]interface{}) PermissionInstance {

	refId, ok := row["reference_id"]
	if !ok {
		refId = row["id"]
	}
	rowType := row["__type"].(string)

	var perm PermissionInstance

	if rowType != "usergroup" {
		if row[USER_ACCOUNT_ID_COLUMN] != nil {
			uid, _ := row[USER_ACCOUNT_ID_COLUMN].(string)
			perm.UserId = uid
		} else {
			row, _ = dr.GetReferenceIdToObject(rowType, refId.(string))
			u := row[USER_ACCOUNT_ID_COLUMN]
			if u != nil {
				uid, _ := u.(string)
				perm.UserId = uid
			}
		}

	}

	loc := strings.Index(rowType, "_has_")
	//log.Infof("Location [%v]: %v", dr.model.GetName(), loc)
	if loc == -1 && dr.Cruds[rowType].model.HasMany("usergroup") {

		perm.UserGroupId = dr.GetObjectUserGroupsByWhere(rowType, "reference_id", refId.(string))

	} else if rowType == "usergroup" {
		originalGroupId, _ := row["reference_id"]
		originalGroupIdStr := refId.(string)
		if originalGroupId != nil {
			originalGroupIdStr = originalGroupId.(string)
		}

		perm.UserGroupId = []auth.GroupPermission{
			{
				GroupReferenceId:    originalGroupIdStr,
				ObjectReferenceId:   refId.(string),
				RelationReferenceId: refId.(string),
				Permission:          auth.AuthPermission(dr.Cruds["usergroup"].model.GetDefaultPermission()),
			},
		}
	} else if loc > -1 {
		// this is a something belongs to a usergroup row
		//for colName, colValue := range row {
		//	if EndsWithCheck(colName, "_id") && colName != "reference_id" {
		//		if colName != "usergroup_id" {
		//			return dr.GetObjectPermissionByReferenceId(strings.Split(rowType, "_"+colName)[0], colValue.(string))
		//		}
		//	}
		//}

	}

	rowPermission := row["permission"]
	if rowPermission != nil {

		var err error
		i64, ok := rowPermission.(int64)
		if !ok {
			f64, ok := rowPermission.(float64)
			if !ok {
				i64, err = strconv.ParseInt(rowPermission.(string), 10, 64)
				//p, err := int64(row["permission"].(int))
				if err != nil {
					log.Errorf("Invalid cast :%v", err)
				}
			} else {
				i64 = int64(f64)
			}
		}

		perm.Permission = auth.AuthPermission(i64)
	} else {
		pe := dr.GetObjectPermissionByReferenceId(rowType, refId.(string))
		perm.Permission = pe.Permission
	}
	//log.Infof("Row permission: %v  ---------------- %v", perm, row)
	return perm
}

func (dr *DbResource) GetRowsByWhereClause(typeName string, where ...squirrel.Eq) ([]map[string]interface{}, [][]map[string]interface{}, error) {

	stmt := statementbuilder.Squirrel.Select("*").From(typeName)

	for _, w := range where {
		stmt = stmt.Where(w)
	}

	s, q, err := stmt.ToSql()

	//log.Infof("Select query: %v == [%v]", s, q)
	rows, err := dr.db.Queryx(s, q...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	m1, include, err := dr.ResultToArrayOfMap(rows, dr.Cruds[typeName].model.GetColumnMap(), map[string]bool{"*": true})

	return m1, include, err

}

func (dr *DbResource) GetUserGroupIdByUserId(userId int64) uint64 {

	s, q, err := statementbuilder.Squirrel.Select("usergroup_id").From("user_account_user_account_id_has_usergroup_usergroup_id").Where(squirrel.NotEq{"usergroup_id": 1}).Where(squirrel.Eq{"user_account_id": userId}).OrderBy("created_at").Limit(1).ToSql()
	if err != nil {
		log.Errorf("Failed to create sql query: %v", err)
		return 0
	}

	var refId uint64

	err = dr.db.QueryRowx(s, q...).Scan(&refId)
	if err != nil {
		log.Errorf("Failed to scan user group id from the result 1: %v", err)
	}

	return refId

}
func (dr *DbResource) GetUserIdByUsergroupId(usergroupId int64) string {

	s, q, err := statementbuilder.Squirrel.Select("u.reference_id").From("user_account_user_account_id_has_usergroup_usergroup_id uu").LeftJoin("user_account u on uu.user_account_id = u.id").Where(squirrel.Eq{"uu.usergroup_id": usergroupId}).OrderBy("uu.created_at").Limit(1).ToSql()
	if err != nil {
		log.Errorf("Failed to create sql query: %v", err)
		return ""
	}

	var refId string

	err = dr.db.QueryRowx(s, q...).Scan(&refId)
	if err != nil {
		//log.Errorf("Failed to execute query: %v == %v", s, q)
		//log.Errorf("Failed to scan user group id from the result 2: %v", err)
	}

	return refId

}

func (dr *DbResource) GetUserEmailIdByUsergroupId(usergroupId int64) string {

	s, q, err := statementbuilder.Squirrel.Select("u.email").From("user_account_user_account_id_has_usergroup_usergroup_id uu").
		LeftJoin(USER_ACCOUNT_TABLE_NAME + " u on uu." + USER_ACCOUNT_ID_COLUMN + " = u.id").Where(squirrel.Eq{"uu.usergroup_id": usergroupId}).
		OrderBy("uu.created_at").Limit(1).ToSql()
	if err != nil {
		log.Errorf("Failed to create sql query: %v", err)
		return ""
	}

	var email string

	err = dr.db.QueryRowx(s, q...).Scan(&email)
	if err != nil {
		log.Errorf("Failed to execute query: %v == %v", s, q)
		log.Errorf("Failed to scan user group id from the result 3: %v", err)
	}

	return email

}

func (dr *DbResource) GetSingleRowByReferenceId(typeName string, referenceId string) (map[string]interface{}, []map[string]interface{}, error) {
	//log.Infof("Get single row by id: [%v][%v]", typeName, referenceId)
	s, q, err := statementbuilder.Squirrel.Select("*").From(typeName).Where(squirrel.Eq{"reference_id": referenceId}).ToSql()
	if err != nil {
		log.Errorf("Failed to create select query by ref id: %v", referenceId)
		return nil, nil, err
	}

	rows, err := dr.db.Queryx(s, q...)
	defer rows.Close()
	resultRows, includeRows, err := dr.ResultToArrayOfMap(rows, dr.Cruds[typeName].model.GetColumnMap(), map[string]bool{"*": true})
	if err != nil {
		return nil, nil, err
	}

	if len(resultRows) < 1 {
		return nil, nil, errors.New("No such entity")
	}

	m := resultRows[0]
	n := includeRows[0]

	return m, n, err

}

func (dr *DbResource) GetSingleRowById(typeName string, id int64) (map[string]interface{}, []map[string]interface{}, error) {
	//log.Infof("Get single row by id: [%v][%v]", typeName, referenceId)
	s, q, err := statementbuilder.Squirrel.Select("*").From(typeName).Where(squirrel.Eq{"id": id}).ToSql()
	if err != nil {
		log.Errorf("Failed to create select query by id: %v", id)
		return nil, nil, err
	}

	rows, err := dr.db.Queryx(s, q...)
	defer rows.Close()
	resultRows, includeRows, err := dr.ResultToArrayOfMap(rows, dr.Cruds[typeName].model.GetColumnMap(), map[string]bool{"*": true})
	if err != nil {
		return nil, nil, err
	}

	if len(resultRows) < 1 {
		return nil, nil, errors.New("No such entity")
	}

	m := resultRows[0]
	n := includeRows[0]

	return m, n, err

}

func (dr *DbResource) GetObjectByWhereClause(typeName string, column string, val interface{}) (map[string]interface{}, error) {
	s, q, err := statementbuilder.Squirrel.Select("*").From(typeName).Where(squirrel.Eq{column: val}).ToSql()
	if err != nil {
		return nil, err
	}

	row, err := dr.db.Queryx(s, q...)

	if err != nil {
		return nil, err
	}
	defer row.Close()

	m, _, err := dr.ResultToArrayOfMap(row, dr.Cruds[typeName].model.GetColumnMap(), nil)

	if len(m) == 0 {
		log.Infof("No result found for [%v] [%v][%v]", typeName, column, val)
		return nil, err
	}

	return m[0], err
}

func (dr *DbResource) GetIdToObject(typeName string, id int64) (map[string]interface{}, error) {
	s, q, err := statementbuilder.Squirrel.Select("*").From(typeName).Where(squirrel.Eq{"id": id}).ToSql()
	if err != nil {
		return nil, err
	}

	row, err := dr.db.Queryx(s, q...)

	if err != nil {
		return nil, err
	}
	defer row.Close()

	m, _, err := dr.ResultToArrayOfMap(row, dr.Cruds[typeName].model.GetColumnMap(), nil)

	if len(m) == 0 {
		log.Infof("No result found for [%v][%v]", typeName, id)
		return nil, err
	}

	return m[0], err
}

func (dr *DbResource) TruncateTable(typeName string) error {
	log.Printf("Truncate table: %v", typeName)

	s, q, err := statementbuilder.Squirrel.Delete(typeName).ToSql()
	if err != nil {
		return err
	}

	_, err = dr.db.Exec(s, q...)
	return err

}

// Update the data and set the values using the data map without an validation or transformations
// Invoked by data import action
func (dr *DbResource) DirectInsert(typeName string, data map[string]interface{}) error {
	var err error

	columnMap := dr.Cruds[typeName].model.GetColumnMap()

	cols := make([]string, 0)
	vals := make([]interface{}, 0)
	for columnName := range columnMap {
		colInfo, ok := dr.tableInfo.GetColumnByName(columnName)
		if !ok {
			continue
		}
		value := data[columnName]
		switch colInfo.ColumnType {
		case "datetime":
			if value != nil {
				value, err = dateparse.ParseLocal(value.(string))
				if err != nil {
					log.Errorf("Failed to parse value as time, insert will fail [%v][%v]: %v", columnName, value, err)
					continue
				}
			}
		}
		cols = append(cols, columnName)
		vals = append(vals, value)

	}

	sqlString, args, err := statementbuilder.Squirrel.Insert(typeName).Columns(cols...).Values(vals...).ToSql()

	if err != nil {
		return err
	}

	_, err = dr.db.Exec(sqlString, args...)
	return err
}

// Get all rows from the table `typeName`
// Returns an array of Map object, each object has the column name to value mapping
// Utility method for loading all objects having low count
// Can be used by actions
func (dr *DbResource) GetAllObjects(typeName string) ([]map[string]interface{}, error) {
	s, q, err := statementbuilder.Squirrel.Select("*").From(typeName).ToSql()
	if err != nil {
		return nil, err
	}

	row, err := dr.db.Queryx(s, q...)

	if err != nil {
		return nil, err
	}
	defer row.Close()

	m, _, err := dr.ResultToArrayOfMap(row, dr.Cruds[typeName].model.GetColumnMap(), nil)

	return m, err
}

// Get all rows from the table `typeName`
// Returns an array of Map object, each object has the column name to value mapping
// Utility method for loading all objects having low count
// Can be used by actions
func (dr *DbResource) GetAllObjectsWithWhere(typeName string, where ...squirrel.Eq) ([]map[string]interface{}, error) {
	query := statementbuilder.Squirrel.Select("*").From(typeName)

	for _, w := range where {
		query = query.Where(w)
	}

	s, q, err := query.ToSql()
	if err != nil {
		return nil, err
	}

	row, err := dr.db.Queryx(s, q...)

	if err != nil {
		return nil, err
	}
	defer row.Close()

	m, _, err := dr.ResultToArrayOfMap(row, dr.Cruds[typeName].model.GetColumnMap(), nil)

	return m, err
}

// Get all rows from the table `typeName` without any processing of the response
// expect no "__type" column on the returned instances
// Returns an array of Map object, each object has the column name to value mapping
// Utility method for loading all objects having low count
// Can be used by actions
func (dr *DbResource) GetAllRawObjects(typeName string) ([]map[string]interface{}, error) {
	s, q, err := statementbuilder.Squirrel.Select("*").From(typeName).ToSql()
	if err != nil {
		return nil, err
	}

	row, err := dr.db.Queryx(s, q...)

	if err != nil {
		return nil, err
	}
	defer row.Close()

	m, err := RowsToMap(row, typeName)

	return m, err
}

// Load an object of type `typeName` using a reference_id
// Used internally, can be used by actions
func (dr *DbResource) GetReferenceIdToObject(typeName string, referenceId string) (map[string]interface{}, error) {
	//log.Infof("Get Object by reference id [%v][%v]", typeName, referenceId)
	s, q, err := statementbuilder.Squirrel.Select("*").From(typeName).Where(squirrel.Eq{"reference_id": referenceId}).ToSql()
	if err != nil {
		return nil, err
	}

	//log.Infof("Get object by reference id sql: %v", s)
	row, err := dr.db.Queryx(s, q...)

	if err != nil {
		return nil, err
	}
	defer row.Close()

	results, _, err := dr.ResultToArrayOfMap(row, dr.Cruds[typeName].model.GetColumnMap(), nil)
	if err != nil {
		return nil, err
	}

	//log.Infof("Have to return first of %d results", len(results))
	if len(results) == 0 {
		return nil, fmt.Errorf("no such object [%v][%v]", typeName, referenceId)
	}

	return results[0], err
}

// Load rows from the database of `typeName` with a where clause to filter rows
// Converts the queries to sql and run query with where clause
// Returns list of reference_ids
func (dr *DbResource) GetReferenceIdByWhereClause(typeName string, queries ...squirrel.Eq) ([]string, error) {
	builder := statementbuilder.Squirrel.Select("reference_id").From(typeName)

	for _, qu := range queries {
		builder = builder.Where(qu)
	}

	s, q, err := builder.ToSql()
	log.Debugf("reference id by where query: %v", s)

	if err != nil {
		return nil, err
	}

	res, err := dr.db.Queryx(s, q...)

	if err != nil {
		return nil, err
	}
	defer res.Close()

	ret := make([]string, 0)
	for res.Next() {
		var s string
		res.Scan(&s)
		ret = append(ret, s)
	}

	return ret, err

}

// Load rows from the database of `typeName` with a where clause to filter rows
// Converts the queries to sql and run query with where clause
// Returns  list of internal database integer ids
func (dr *DbResource) GetIdByWhereClause(typeName string, queries ...squirrel.Eq) ([]int64, error) {
	builder := statementbuilder.Squirrel.Select("id").From(typeName)

	for _, qu := range queries {
		builder = builder.Where(qu)
	}

	s, q, err := builder.ToSql()
	log.Debugf("reference id by where query: %v", s)

	if err != nil {
		return nil, err
	}

	res, err := dr.db.Queryx(s, q...)

	if err != nil {
		return nil, err
	}
	defer res.Close()

	ret := make([]int64, 0)
	for res.Next() {
		var s int64
		res.Scan(&s)
		ret = append(ret, s)
	}

	return ret, err

}

// Lookup an integer id and return a string reference id of an object of type `typeName`
func (dr *DbResource) GetIdToReferenceId(typeName string, id int64) (string, error) {

	s, q, err := statementbuilder.Squirrel.Select("reference_id").From(typeName).Where(squirrel.Eq{"id": id}).ToSql()
	if err != nil {
		return "", err
	}

	var str string
	row := dr.db.QueryRowx(s, q...)
	err = row.Scan(&str)
	return str, err

}

// Lookup an string reference id and return a internal integer id of an object of type `typeName`
func (dr *DbResource) GetReferenceIdToId(typeName string, referenceId string) (int64, error) {

	var id int64
	s, q, err := statementbuilder.Squirrel.Select("id").From(typeName).Where(squirrel.Eq{"reference_id": referenceId}).ToSql()
	if err != nil {
		return 0, err
	}

	err = dr.db.QueryRowx(s, q...).Scan(&id)
	return id, err

}

// select "column" from "typeName" where matchColumn in (values)
// returns list of values of the column
func (dr *DbResource) GetSingleColumnValueByReferenceId(typeName string, selectColumn []string, matchColumn string, values []string) ([]interface{}, error) {

	s, q, err := statementbuilder.Squirrel.Select(selectColumn...).From(typeName).Where(squirrel.Eq{matchColumn: values}).ToSql()
	if err != nil {
		return nil, err
	}

	rows := dr.db.QueryRowx(s, q...)
	return rows.SliceScan()
}

// convert the result of db.QueryRowx => rows to array of data
// can be used on any *sqlx.Rows and assign a typeName
func RowsToMap(rows *sqlx.Rows, typeName string) ([]map[string]interface{}, error) {

	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	responseArray := make([]map[string]interface{}, 0)

	for rows.Next() {

		rc := NewMapStringScan(columns)
		err := rc.Update(rows)
		if err != nil {
			return responseArray, err
		}

		dbRow := rc.Get()
		dbRow["__type"] = typeName
		responseArray = append(responseArray, dbRow)
	}

	return responseArray, nil
}

// convert the result of db.QueryRowx => rows to array of data
// fetches the related objects also
// expects columnMap to be fetched from rows
// check usage in exiting source for example
// includeRelationMap can be nil to include none or map[string]bool{"*": true} to include all relations
// can be used on any *sqlx.Rows
func (dr *DbResource) ResultToArrayOfMap(rows *sqlx.Rows, columnMap map[string]api2go.ColumnInfo, includedRelationMap map[string]bool) ([]map[string]interface{}, [][]map[string]interface{}, error) {

	//finalArray := make([]map[string]interface{}, 0)

	responseArray, err := RowsToMap(rows, dr.model.GetName())
	if err != nil {
		return responseArray, nil, err
	}

	objMap := make(map[string]interface{})
	includes := make([][]map[string]interface{}, 0)

	for _, row := range responseArray {
		localInclude := make([]map[string]interface{}, 0)

		for key, val := range row {
			//log.Infof("Key: [%v] == %v", key, val)

			columnInfo, ok := columnMap[key]
			if !ok {
				continue
			}

			if val != nil && columnInfo.ColumnType == "datetime" {

				stringVal, ok := val.(string)
				if ok {
					parsedValue, _, err := fieldtypes.GetTime(stringVal)
					if err != nil {
						parsedValue, _, err := fieldtypes.GetDateTime(stringVal)
						if InfoErr(err, "Failed to parse date time from [%v]: %v", columnInfo.ColumnName, stringVal) {
							row[key] = nil
						} else {
							row[key] = parsedValue
						}
					} else {
						row[key] = parsedValue
					}
				}
			}

			if !columnInfo.IsForeignKey {
				continue
			}

			if val == "" || val == nil {
				continue
			}

			namespace := columnInfo.ForeignKeyData.Namespace
			//log.Infof("Resolve foreign key from [%v][%v][%v]", columnInfo.ForeignKeyData.DataSource, namespace, val)
			switch columnInfo.ForeignKeyData.DataSource {
			case "self":
				referenceIdInt, ok := val.(int64)
				if !ok {
					stringIntId := val.(string)
					referenceIdInt, err = strconv.ParseInt(stringIntId, 10, 64)
					CheckErr(err, "Failed to convert string id to int id")
				}
				cache_key := fmt.Sprintf("%v-%v", namespace, referenceIdInt)
				objCached, ok := objMap[cache_key]
				if ok {
					localInclude = append(localInclude, objCached.(map[string]interface{}))
					continue
				}

				refId, err := dr.GetIdToReferenceId(namespace, referenceIdInt)

				if err != nil {
					log.Errorf("Failed to get ref id for [%v][%v]: %v", namespace, val, err)
					continue
				}
				row[key] = refId

				if includedRelationMap != nil && (includedRelationMap[namespace] || includedRelationMap["*"]) {
					obj, err := dr.GetIdToObject(namespace, referenceIdInt)
					obj["__type"] = namespace

					if err != nil {
						log.Errorf("Failed to get ref object for [%v][%v]: %v", namespace, val, err)
					} else {
						localInclude = append(localInclude, obj)
					}
				}

			case "cloud_store":
				referenceStorageInformation := val.(string)
				log.Infof("Resolve files from cloud store: %v", referenceStorageInformation)
				foreignFilesList := make([]map[string]interface{}, 0)
				err := json.Unmarshal([]byte(referenceStorageInformation), &foreignFilesList)
				CheckErr(err, "Failed to obtain list of file information")
				if err != nil {
					continue
				}

				for _, file := range foreignFilesList {
					file["src"] = file["name"].(string)
				}

				row[key] = foreignFilesList
				log.Infof("set row[%v]  == %v", key, foreignFilesList)
				if err != nil {
					log.Errorf("Failed to get ref id for [%v][%v]: %v", namespace, val, err)
					continue
				}

				if includedRelationMap != nil && (includedRelationMap[columnInfo.ColumnName] || includedRelationMap["*"]) {

					resolvedFilesList, err := dr.GetFileFromLocalCloudStore(dr.TableInfo().TableName, columnInfo.ColumnName, foreignFilesList)
					CheckErr(err, "Failed to resolve file from cloud store")
					row[key] = resolvedFilesList
					for _, file := range resolvedFilesList {
						file["__type"] = columnInfo.ColumnType
						localInclude = append(localInclude, file)
					}

				}
			default:
				log.Errorf("Undefined data source: %v", columnInfo.ForeignKeyData.DataSource)
				continue
			}

		}

		includes = append(includes, localInclude)

	}

	return responseArray, includes, nil
}

// convert the result of db.QueryRowx => rows to array of data
// can be used on any *sqlx.Rows and assign a typeName
// calls RowsToMap with the current model name
func (dr *DbResource) ResultToArrayOfMapRaw(rows *sqlx.Rows, columnMap map[string]api2go.ColumnInfo) ([]map[string]interface{}, error) {

	//finalArray := make([]map[string]interface{}, 0)

	responseArray, err := RowsToMap(rows, dr.model.GetName())
	if err != nil {
		return responseArray, err
	}

	return responseArray, nil
}

// resolve a file column from data in column to actual file on a cloud store
// returns a map containing the metadata of the file and the file contents as base64 encoded
// can be sent to browser to invoke downloading js and data urls
func (resource *DbResource) GetFileFromCloudStore(data api2go.ForeignKeyData, filesList []map[string]interface{}) (resp []map[string]interface{}, err error) {

	cloudStore, err := resource.GetCloudStoreByName(data.Namespace)
	if err != nil {
		return resp, err
	}

	for _, fileItem := range filesList {
		newFileItem := make(map[string]interface{})

		for key, val := range fileItem {
			newFileItem[key] = val
		}

		fileName := fileItem["name"].(string)
		bytes, err := ioutil.ReadFile(cloudStore.RootPath + "/" + data.KeyName + "/" + fileName)
		CheckErr(err, "Failed to read file on storage")
		if err != nil {
			continue
		}
		newFileItem["reference_id"] = fileItem["name"]
		newFileItem["contents"] = base64.StdEncoding.EncodeToString(bytes)
		resp = append(resp, newFileItem)
	}
	return resp, nil
}

// resolve a file column from data in column to actual file on a cloud store
// returns a map containing the metadata of the file and the file contents as base64 encoded
// can be sent to browser to invoke downloading js and data urls
func (resource *DbResource) GetFileFromLocalCloudStore(tableName string, columnName string, filesList []map[string]interface{}) (resp []map[string]interface{}, err error) {

	assetFolder, ok := resource.AssetFolderCache[tableName][columnName]
	if !ok {
		return nil, errors.New("not a synced folder")
	}

	for _, fileItem := range filesList {
		newFileItem := make(map[string]interface{})

		for key, val := range fileItem {
			newFileItem[key] = val
		}

		filePath := fileItem["src"].(string)
		bytes, err := ioutil.ReadFile(assetFolder.LocalSyncPath + "/" + filePath)
		CheckErr(err, "Failed to read file on storage [%v]: %v", assetFolder.LocalSyncPath, filePath)
		if err != nil {
			continue
		}
		newFileItem["reference_id"] = fileItem["name"]
		newFileItem["contents"] = base64.StdEncoding.EncodeToString(bytes)
		resp = append(resp, newFileItem)
	}
	return resp, nil
}
