package push

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ego-component/egorm"
	"github.com/gotomicro/ego/core/econf"

	"github.com/clickvisual/clickvisual/api/pkg/model/db"
	"github.com/clickvisual/clickvisual/api/pkg/model/view"
)

const (
	ChannelDingDing = iota + 1
	ChannelWeChat
	ChannelFeiShu
	ChannelSlack
	ChannelEmail
	ChannelTelegram
)

type Operator interface {
	Send(notification view.Notification, alarm *db.Alarm, channel *db.AlarmChannel, oneTheLogs string) (err error)
}

func Instance(typ int) (Operator, error) {
	var err error
	switch typ {
	case ChannelDingDing:
		return &DingDing{}, nil
	case ChannelWeChat:
		return &WeChat{}, nil
	case ChannelFeiShu:
		return &FeiShu{}, nil
	case ChannelSlack:
		return &Slack{}, nil
	case ChannelEmail:
		return &Email{}, nil
	case ChannelTelegram:
		return &Telegram{}, nil
	default:
		err = errors.New("undefined channels")
	}
	return nil, err
}

// transformToMarkdown
// Description: 提供一个通用的md模式的获取内容的方法
// param notification  通知的部分方法
// param alarm 警告的数据库连接
// param oneTheLogs 日志内容
// return title 标题
// return text 内容
// return err 错误
func transformToMarkdown(notification view.Notification, alarm *db.Alarm, channel *db.AlarmChannel, oneTheLogs string) (title, text string, err error) {
	groupKey := notification.GroupKey
	status := notification.Status
	annotations := notification.CommonAnnotations

	var buffer bytes.Buffer
	buffer.WriteString("### ClickVisual 告警\n")
	buffer.WriteString(fmt.Sprintf("##### 告警名称: %s\n", alarm.Name))
	if alarm.Desc != "" {
		buffer.WriteString(fmt.Sprintf("##### 告警描述: %s\n", alarm.Desc))
	}
	condsFilter := egorm.Conds{}
	condsFilter["alarm_id"] = alarm.ID
	filters, err := db.AlarmFilterList(condsFilter)
	if err != nil {
		return
	}
	var exp string
	if len(filters) == 1 {
		exp = filters[0].When
	}
	user, _ := db.UserInfo(alarm.Uid)
	ins, table, _, _ := db.GetAlarmTableInstanceInfo(alarm.ID)
	for _, alert := range notification.Alerts {
		end := alert.StartsAt.Add(time.Minute).Unix()
		start := alert.StartsAt.Add(-db.UnitMap[alarm.Unit].Duration - time.Minute).Unix()
		annotations = alert.Annotations
		if exp != "" {
			buffer.WriteString(fmt.Sprintf("##### 表达式: %s\n\n", exp))
		}
		buffer.WriteString(fmt.Sprintf("##### 首次触发时间：%s\n", alert.StartsAt.Add(time.Hour*8).Format("2006-01-02 15:04:05")))
		buffer.WriteString(fmt.Sprintf("##### 相关实例：%s %s\n", ins.Name, ins.Desc))
		buffer.WriteString(fmt.Sprintf("##### 相关日志库：%s %s\n", table.Name, table.Desc))
		if status == "resolved" {
			buffer.WriteString("##### 状态：<font color=#008000>已恢复</font>\n")
		} else {
			buffer.WriteString("##### 状态：：<font color=red>告警中</font>\n")
		}
		buffer.WriteString(fmt.Sprintf("##### 创建人 ：%s(%s)\n", user.Username, user.Nickname))

		buffer.WriteString(fmt.Sprintf("##### %s\n\n", annotations["description"]))

		buffer.WriteString(fmt.Sprintf("##### 详情: %s/alarm/rules/history?id=%d&start=%d&end=%d\n\n",
			strings.TrimRight(econf.GetString("app.rootURL"), "/"), alarm.ID, start, end,
		))
		if oneTheLogs != "" {
			buffer.WriteString(fmt.Sprintf("##### 详情: %s", oneTheLogs))
		}
	}
	return fmt.Sprintf("通知组：%s(当前状态:%s)", groupKey, status), buffer.String(), nil
}
