package service

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ego-component/egorm"
	"github.com/gotomicro/ego/core/elog"
	"gorm.io/gorm"

	"github.com/clickvisual/clickvisual/api/internal/invoker"
	"github.com/clickvisual/clickvisual/api/internal/service/inquiry"
	"github.com/clickvisual/clickvisual/api/internal/service/inquiry/builder/bumo"
	"github.com/clickvisual/clickvisual/api/internal/service/kube"
	"github.com/clickvisual/clickvisual/api/internal/service/kube/resource"
	"github.com/clickvisual/clickvisual/api/pkg/constx"
	"github.com/clickvisual/clickvisual/api/pkg/model/db"
	"github.com/clickvisual/clickvisual/api/pkg/model/view"
)

const prometheusRuleTemplate = `groups:
- name: default
  rules:
  - alert: %s
    expr: %s
    for: %s
    labels:
      severity: warning
    annotations:
      summary: "告警 {{ $labels.name }}"
      description: "{{ $labels.desc }}  (当前值: {{ $value }})"`

const (
	reloadTimes    = 30
	reloadInterval = time.Second * 10
)

type alarm struct {
	reloadChan chan int64
}

// NewAlarm ...
func NewAlarm() *alarm {
	a := &alarm{
		reloadChan: make(chan int64, reloadTimes),
	}
	go func() {
		for r := range a.reloadChan {
			invoker.Logger.Info("AllPrometheusReload", elog.Int("times", len(a.reloadChan)), elog.Int64("r", r), elog.Int64("now", time.Now().Unix()))
			AllPrometheusReload()
			time.Sleep(reloadInterval)
		}
	}()
	return a
}

func (i *alarm) FilterCreate(tx *gorm.DB, alarmObj *db.Alarm, filters []view.ReqAlarmFilterCreate) (res map[int]view.AlarmFilterItem, err error) {
	res = make(map[int]view.AlarmFilterItem, 0)
	for _, filter := range filters {
		// create filter
		filterObj := &db.AlarmFilter{
			AlarmId:        alarmObj.ID,
			Tid:            filter.Tid,
			When:           filter.When,
			SetOperatorTyp: filter.SetOperatorTyp,
			SetOperatorExp: filter.SetOperatorExp,
			Mode:           filter.Mode,
		}
		if filterObj.When == "" {
			filterObj.When = "1=1"
		}
		err = db.AlarmFilterCreate(tx, filterObj)
		if err != nil {
			return
		}
		row := view.AlarmFilterItem{
			AlarmFilter: filterObj,
		}
		// create condition
		row.Exp, err = i.ConditionCreate(tx, alarmObj, filter.Conditions, filterObj.ID)
		if err != nil {
			invoker.Logger.Error("alarm", elog.FieldName("conditionCreate"), elog.String("err", err.Error()))
			return
		}
		res[filterObj.ID] = row
	}
	return
}

func (i *alarm) ConditionCreate(tx *gorm.DB, obj *db.Alarm, conditions []view.ReqAlarmConditionCreate, filterId int) (exp string, err error) {
	expVal := fmt.Sprintf("%s{%s} offset 10s", bumo.PrometheusMetricName, inquiry.TagsToString(obj, false, filterId))
	sort.Slice(conditions, func(i, j int) bool {
		return conditions[i].SetOperatorTyp < conditions[j].SetOperatorTyp
	})
	for _, condition := range conditions {
		var innerCond string
		switch condition.Cond {
		case 0:
			innerCond = fmt.Sprintf("%s>%d", expVal, condition.Val1)
		case 1:
			innerCond = fmt.Sprintf("%s<%d", expVal, condition.Val1)
		case 2:
			innerCond = fmt.Sprintf("(%s<%d or %s>%d)", expVal, condition.Val1, expVal, condition.Val2)
		case 3:
			innerCond = fmt.Sprintf("(%s>=%d and %s<=%d)", expVal, condition.Val1, expVal, condition.Val2)
		}
		switch condition.SetOperatorTyp {
		case 0:
			exp = innerCond
		case 1:
			if exp == "" {
				err = errors.New("conditions error")
				return
			}
			exp = fmt.Sprintf("%s and %s", exp, innerCond)
		case 2:
			if exp == "" {
				err = errors.New("conditions error")
				return
			}
			exp = fmt.Sprintf("%s or %s", exp, innerCond)
		}
		conditionObj := &db.AlarmCondition{
			AlarmId:        obj.ID,
			FilterId:       filterId,
			SetOperatorTyp: condition.SetOperatorTyp,
			SetOperatorExp: condition.SetOperatorExp,
			Cond:           condition.Cond,
			Val1:           condition.Val1,
			Val2:           condition.Val2,
		}
		err = db.AlarmConditionCreate(tx, conditionObj)
		if err != nil {
			return
		}
	}

	// empty data alert
	exp = aggregationOp(obj.Mode, exp, expVal)
	exp = noDataOp(obj.NoDataOp, exp, expVal)
	return
}

const (
	NoDataOpDefault = 0
	NoDataOpOK      = 1
	NoDataOpAlert   = 2
)

func aggregationOp(mode int, exp string, expVal string) string {
	switch mode {
	case db.AlarmModeAggregation:
		return fmt.Sprintf("%s and %s!=-1", exp, expVal)
	default:
		return exp
	}
}

func noDataOp(op int, exp, expVal string) string {
	switch op {
	case NoDataOpDefault:
		return exp
	case NoDataOpOK:
		return fmt.Sprintf("(%s) or absent(%s)!=1", exp, expVal)
	case NoDataOpAlert:
		return fmt.Sprintf("(%s) or absent(%s)==1", exp, expVal)
	default:
		return exp
	}
}

func (i *alarm) PrometheusReload(prometheusTarget string) (err error) {
	resp, err := http.Post(strings.TrimSuffix(prometheusTarget, "/")+"/-/reload", "text/html;charset=utf-8", nil)
	if err != nil {
		invoker.Logger.Error("reload", elog.Any("reload", prometheusTarget+"/-/reload"), elog.Any("err", err.Error()))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	return
}

func (i *alarm) PrometheusRuleGen(obj *db.Alarm, exp string) (rule string, err error) {
	rule = fmt.Sprintf(prometheusRuleTemplate, obj.AlertUniqueName(), exp, obj.AlertInterval())
	return
}

func (i *alarm) PrometheusRuleCreateOrUpdate(instance db.BaseInstance, ruleName, rule string) (err error) {
	switch instance.RuleStoreType {
	case db.RuleStoreTypeFile:
		content := []byte(rule)
		path := strings.TrimSuffix(instance.FilePath, "/")
		err = os.WriteFile(path+"/"+ruleName, content, 0644)
		if err != nil {
			return
		}
	case db.RuleStoreTypeK8s:
		client, errCluster := kube.ClusterManager.GetClusterManager(instance.ClusterId)
		if errCluster != nil {
			return errCluster
		}
		rules := make(map[string]string)
		rules[ruleName] = rule
		err = resource.ConfigmapCreateOrUpdate(client, instance.Namespace, instance.Configmap, rules)
		if err != nil {
			return
		}
	default:
		return constx.ErrAlarmRuleStoreIsClosed
	}
	i.AddPrometheusReloadChan()
	return nil
}

func (i *alarm) PrometheusRuleDelete(instance *db.BaseInstance, obj *db.Alarm) (err error) {
	if obj.AlertRules == nil || len(obj.AlertRules) == 0 {
		// v1 version
		alarmRuleDelete(instance, obj.AlertRuleName(0))
	} else {
		// v2 version
		for ruleName := range obj.AlertRules {
			alarmRuleDelete(instance, ruleName)
		}
	}
	i.AddPrometheusReloadChan()
	return nil
}

func alarmRuleDelete(instance *db.BaseInstance, ruleName string) {
	switch instance.RuleStoreType {
	case db.RuleStoreTypeK8s:
		invoker.Logger.Debug("alert", elog.Any("instance", instance))
		err := resource.ConfigmapDelete(instance.ClusterId, instance.Namespace, instance.Configmap, ruleName)
		if err != nil {
			invoker.Logger.Error("reload", elog.FieldName("configmapDelete"), elog.Any("ruleName", ruleName), elog.FieldErr(err))
			return
		}
	case db.RuleStoreTypeFile:
		path := strings.TrimSuffix(instance.FilePath, "/")
		err := os.Remove(path + "/" + ruleName)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			invoker.Logger.Error("reload", elog.FieldName("osRemove"), elog.Any("ruleName", ruleName), elog.FieldErr(err))
			return
		}
	}
	return
}

func (i *alarm) CreateOrUpdate(tx *gorm.DB, alarmObj *db.Alarm, req view.ReqAlarmCreate) (err error) {
	// table info
	tableInfo, err := db.TableInfo(tx, alarmObj.Tid)
	if err != nil {
		invoker.Logger.Error("alarm", elog.String("step", "alarm table info"), elog.String("err", err.Error()))
		return
	}

	// v1 -> v2 disable root conditions field
	req.ConvertV2()
	filtersDB, err := i.FilterCreate(tx, alarmObj, req.Filters)
	if err != nil {
		invoker.Logger.Error("alarm", elog.String("step", "alarm create failed 02"), elog.String("err", err.Error()))
		return
	}

	// prometheus set
	instance, err := db.InstanceInfo(tx, tableInfo.Database.Iid)
	if err != nil {
		invoker.Logger.Error("alarm", elog.String("step", "you need to configure alarms related to the instance first:"), elog.String("err", err.Error()))
		return
	}
	op, err := InstanceManager.Load(tableInfo.Database.Iid)
	if err != nil {
		invoker.Logger.Error("alarm", elog.String("step", "alarm create failed 04"), elog.String("err", err.Error()))
		return
	}
	// drop alarm views
	if len(alarmObj.ViewDDLs) > 0 {
		for table := range alarmObj.ViewDDLs {
			if err = op.AlertViewDrop(table, tableInfo.Database.Cluster); err != nil {
				return
			}
		}
	} else {
		if alarmObj.ViewTableName != "" {
			err = op.AlertViewDrop(alarmObj.ViewTableName, tableInfo.Database.Cluster)
			if err != nil {
				invoker.Logger.Error("alarm", elog.String("step", "alarm create failed 05"), elog.String("err", err.Error()))
				return
			}
		}
	}
	// create new views
	viewDDLs := db.String2String{}
	alertRules := db.String2String{}
	for filterId, filterItem := range filtersDB {
		// gen view table name & sql
		table, ddl, errAlertViewGen := op.AlertViewGen(alarmObj, filterId, filterItem.When)
		if errAlertViewGen != nil {
			elog.Error("alarm", elog.FieldComponent("CreateOrUpdate"), elog.FieldName("AlertViewGen"), elog.FieldErr(errAlertViewGen))
			return
		}
		// exec view sql
		if err = op.AlertViewCreate(table, ddl, tableInfo.Database.Cluster); err != nil {
			invoker.Logger.Error("alarm", elog.String("step", "alarm create failed 07"),
				elog.String("viewTableName", table),
				elog.String("viewSQL", ddl),
				elog.String("err", err.Error()))
			return
		}
		viewDDLs[table] = ddl
		// rule store
		rule, errPrometheusRuleGen := i.PrometheusRuleGen(alarmObj, filterItem.Exp)
		if errPrometheusRuleGen != nil {
			invoker.Logger.Error("alarm", elog.String("step", "alarm create failed 08"), elog.FieldErr(errPrometheusRuleGen))
			return
		}
		ruleName := alarmObj.AlertRuleName(filterId)
		alertRules[ruleName] = rule
		if err = i.PrometheusRuleCreateOrUpdate(instance, ruleName, rule); err != nil {
			invoker.Logger.Error("alarm", elog.String("step", "alarm create failed 09"), elog.FieldErr(err))
			return
		}
	}

	ups := make(map[string]interface{}, 0)
	ups["alert_rules"] = alertRules
	ups["rule_store_type"] = instance.RuleStoreType
	ups["view_ddl_s"] = viewDDLs
	ups["status"] = db.AlarmStatusOpen
	return db.AlarmUpdate(tx, alarmObj.ID, ups)
}

func (i *alarm) OpenOperator(id int) (err error) {
	instanceInfo, tableInfo, alarmInfo, err := db.GetAlarmTableInstanceInfo(id)
	if err != nil {
		return
	}
	op, errInstanceManager := InstanceManager.Load(instanceInfo.ID)
	if errInstanceManager != nil {
		return
	}
	if len(alarmInfo.ViewDDLs) > 0 {
		for table, ddl := range alarmInfo.ViewDDLs {
			if err = op.AlertViewCreate(table, ddl, tableInfo.Database.Cluster); err != nil {
				return
			}
		}
	} else {
		if err = op.AlertViewCreate(alarmInfo.ViewTableName, alarmInfo.View, tableInfo.Database.Cluster); err != nil {
			return
		}
	}
	for ruleName, alertRule := range alarmInfo.AlertRules {
		if err = i.PrometheusRuleCreateOrUpdate(instanceInfo, ruleName, alertRule); err != nil {
			invoker.Logger.Error("alarm", elog.String("step", "prometheus rule delete failed"), elog.String("err", err.Error()))
			return
		}
	}

	if err = db.AlarmUpdate(invoker.Db, id, map[string]interface{}{"status": db.AlarmStatusOpen}); err != nil {
		return
	}
	return
}

func (i *alarm) Update(uid, alarmId int, req view.ReqAlarmCreate) (err error) {
	if req.Name == "" || req.Interval == 0 || len(req.ChannelIds) == 0 {
		return errors.New("parameter error")
	}
	if len(req.Filters) > 0 {
		req.Mode = req.Filters[0].Mode
	}
	tx := invoker.Db.Begin()
	ups := make(map[string]interface{}, 0)
	ups["name"] = req.Name
	ups["desc"] = req.Desc
	ups["interval"] = req.Interval
	ups["unit"] = req.Unit
	ups["uid"] = uid
	ups["no_data_op"] = req.NoDataOp
	ups["mode"] = req.Mode
	ups["level"] = req.Level
	ups["channel_ids"] = db.Ints(req.ChannelIds)
	if len(req.Filters) > 0 {
		ups["tid"] = req.Filters[0].Tid
	}
	tableIds := db.Ints{}
	for _, f := range req.Filters {
		tableIds = append(tableIds, f.Tid)
	}
	ups["table_ids"] = tableIds
	if err = db.AlarmUpdate(tx, alarmId, ups); err != nil {
		tx.Rollback()
		return
	}
	// filter
	if err = db.AlarmFilterDeleteBatch(tx, alarmId); err != nil {
		tx.Rollback()
		return
	}
	// condition
	if err = db.AlarmConditionDeleteBatch(tx, alarmId); err != nil {
		tx.Rollback()
		return
	}
	obj, err := db.AlarmInfo(tx, alarmId)
	if err != nil {
		tx.Rollback()
		return
	}
	if err = i.CreateOrUpdate(tx, &obj, req); err != nil {
		tx.Rollback()
		return
	}
	if err = tx.Commit().Error; err != nil {
		tx.Rollback()
		return
	}
	return
}

func (i *alarm) AddPrometheusReloadChan() {
	// 10 times
	for k := 0; k < reloadTimes; k++ {
		if len(i.reloadChan) < reloadTimes {
			invoker.Logger.Debug("AllPrometheusReload", elog.String("step", "AddPrometheusReloadChan"), elog.Any("k", k))
			i.reloadChan <- time.Now().Unix()
		}
	}
}

func AllPrometheusReload() {
	instances, err := db.InstanceList(egorm.Conds{})
	if err != nil {
		invoker.Logger.Error("AllPrometheusReload", elog.String("step", "InstanceList"), elog.String("error", err.Error()))
		return
	}
	pm := make(map[string]interface{})
	for _, ins := range instances {
		if ins.PrometheusTarget != "" {
			pm[ins.PrometheusTarget] = struct{}{}
		}
	}
	for target := range pm {
		errReload := Alarm.PrometheusReload(target)
		if errReload != nil {
			invoker.Logger.Error("AllPrometheusReload", elog.String("step", "PrometheusReload"), elog.String("error", errReload.Error()))
		}
	}
	return
}

func AlarmAttachInfo(respList []*db.Alarm) []view.RespAlarmList {
	res := make([]view.RespAlarmList, 0)
	for _, a := range respList {
		instanceInfo, tableInfo, alarmInfo, errAlarmInfo := db.GetAlarmTableInstanceInfo(a.ID)
		if errAlarmInfo != nil {
			invoker.Logger.Error("attachInfo", elog.String("error", errAlarmInfo.Error()))
			continue
		}
		if alarmInfo.User == nil || alarmInfo.User.ID == 0 {
			u, _ := db.UserInfo(alarmInfo.Uid)
			alarmInfo.User = &u
		}
		alarmInfo.User.Password = "*"
		res = append(res, view.RespAlarmList{
			Alarm:        &alarmInfo,
			TableName:    tableInfo.Name,
			TableDesc:    tableInfo.Desc,
			Tid:          tableInfo.ID,
			DatabaseName: tableInfo.Database.Name,
			DatabaseDesc: tableInfo.Database.Desc,
			Did:          tableInfo.Did,
			InstanceName: instanceInfo.Name,
			InstanceDesc: instanceInfo.Desc,
			Iid:          instanceInfo.ID,
		})
	}
	return res
}
