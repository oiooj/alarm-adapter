package adapter

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lodastack/log"
	"github.com/lodastack/models"

	"github.com/influxdata/kapacitor/client/v1"
)

const root = "loda"
const schemaURL = "http://%s:9092"

type Kapacitor struct {
	Addrs     []string
	EventAddr string

	mu      sync.RWMutex
	Clients map[string]*client.Client

	Hash *Consistent
}

func NewKapacitor(addrs []string, eventAddr string) *Kapacitor {
	k := &Kapacitor{
		EventAddr: eventAddr,
	}
	k.SetAddr(addrs)
	return k
}

func (k *Kapacitor) SetAddr(addrs []string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	log.Infof("start update old clients: %v", k.Addrs)
	c := NewConsistent()
	clients := make(map[string]*client.Client)
	var fullAddrs []string
	for _, addr := range addrs {
		addr = fmt.Sprintf(schemaURL, addr)
		c.Add(addr)

		config := client.Config{
			URL:     addr,
			Timeout: time.Duration(3) * time.Second,
		}
		c, err := client.New(config)
		if err != nil {
			log.Errorf("new kapacitor %s client failed: %s", addr, err)
			continue
		}
		clients[addr] = c
		fullAddrs = append(fullAddrs, addr)
	}
	k.Addrs = fullAddrs
	k.Clients = clients
	k.Hash = c
	log.Infof("start update clients: %v", k.Addrs)
}

func (k *Kapacitor) Tasks() map[string]client.Task {
	tasks := make(map[string]client.Task)
	for _, url := range k.Addrs {
		k.mu.RLock()
		c, ok := k.Clients[url]
		k.mu.RUnlock()
		if !ok {
			log.Errorf("get cache kapacitor %s client failed", url)
			continue
		}
		var listOpts client.ListTasksOptions
		listOpts.Default()
		listOpts.Limit = -1
		ts, err := c.ListTasks(&listOpts)
		if err != nil {
			log.Errorf("list kapacitor %s client failed: %s", url, err)
			continue
		}
		for _, t := range ts {
			tasks[t.ID] = t
		}
	}
	return tasks
}

func (k *Kapacitor) Work(tasks map[string]client.Task, alarms map[string]models.Alarm) {
	for id, alarm := range alarms {
		if _, ok := tasks[id]; ok {
			continue
		}
		go k.CreateTask(alarm)
	}

	for id, task := range tasks {
		if _, ok := alarms[id]; ok {
			continue
		}
		go k.RemoveTask(task)
	}
}

// Create a new task.
// Errors if the task already exists.
func (k *Kapacitor) CreateTask(alarm models.Alarm) error {
	tick, err := k.genTick(alarm)
	if err != nil {
		log.Errorf("gen tick script failed:%s", err)
		return err
	}
	dbrps := []client.DBRP{
		{
			Database:        alarm.DB,
			RetentionPolicy: alarm.RP,
		},
	}
	status := client.Disabled
	if alarm.Enable == "true" {
		status = client.Enabled
	}

	createOpts := client.CreateTaskOptions{
		ID:         alarm.Version,
		Type:       client.BatchTask,
		DBRPs:      dbrps,
		TICKscript: tick,
		Status:     status,
	}

	url := k.hashKapacitor(alarm.Version)
	k.mu.RLock()
	c, ok := k.Clients[url]
	k.mu.RUnlock()
	if !ok {
		log.Errorf("get cache kapacitor %s client failed", url)
		return fmt.Errorf("get cache kapacitor %s client failed", url)
	}
	log.Infof("create task:%s at %s", alarm.Version, url)
	_, err = c.CreateTask(createOpts)
	if err != nil {
		log.Errorf("create task at %s failed:%s", url, err)
	}
	return err
}

func (k *Kapacitor) RemoveTask(task client.Task) error {
	if !strings.Contains(task.ID, root+models.VersionSep) {
		log.Errorf("this task not belong to loda: %s", task.ID)
		return fmt.Errorf("this task not belong to loda: %s", task.ID)
	}
	log.Infof("delete task:%s", task.ID)
	// try delete the task at all clients
	k.mu.RLock()
	defer k.mu.RUnlock()
	for url, c := range k.Clients {
		go func(id string) {
			err := c.DeleteTask(c.TaskLink(id))
			if err != nil {
				log.Errorf("delete task at %s failed: %s", url, err)
			}
		}(task.ID)
	}
	return nil
}

func (k *Kapacitor) hashKapacitor(id string) string {
	choose, err := k.Hash.Get(id)
	if err != nil {
		log.Errorf("hash get server failed:%s", err)
		if len(k.Addrs) > 0 {
			return k.Addrs[0]
		}
		return ""
	}
	return choose
}

func genTimeLambda(STime, ETime string) string {
	if STime == "" || ETime == "" {
		return ""
	}
	stime, errStime := strconv.Atoi(STime)
	etime, errEtime := strconv.Atoi(ETime)
	if stime == etime || errStime != nil || errEtime != nil {
		log.Warningf("gen time lambda for tick fail, stime: %s, etime: %s", STime, ETime)
		return ""
	}

	condition := "AND"
	if stime > etime {
		condition = "OR"
	}
	return fmt.Sprintf("AND (hour(\"time\") >= %s %s hour(\"time\") <= %s)", STime, condition, ETime)
}

func (k *Kapacitor) genTick(alarm models.Alarm) (string, error) {
	var queryWhere, groupby, offset string
	if alarm.Where != "" {
		queryWhere = "WHERE " + alarm.Where
	}
	timeLambda := genTimeLambda(alarm.STime, alarm.ETime)

	groupby = alarm.GroupBy
	if groupby != "*" {
		groupby = "time(1m,-5s)"
		tags := strings.Split(alarm.GroupBy, ",")
		for _, tag := range tags {
			if tag == "" {
				continue
			}
			groupby = fmt.Sprintf("%s, '%s'", groupby, tag)
		}
		offset = `.align()
.offset(5s)`
	}
	var res string
	switch alarm.Trigger {
	case models.Relative:
		batch := `
batch
    |query('''
        SELECT (max("value")-min("value")) as diff
        FROM "%s"."%s"."%s" %s
    ''')
        .period(%s)
        .every(%s)
        .groupBy(%s)
        %s
    |alert()
        .crit(lambda: "diff" %s %s %s)
        .post('%s?version=%s')`
		res = fmt.Sprintf(batch, alarm.DB, alarm.RP, alarm.Measurement, queryWhere, alarm.Period, alarm.Every,
			groupby, offset, alarm.Expression, alarm.Value, timeLambda, k.EventAddr, alarm.Version)

	case models.ThresHold:
		batch := `
batch
    |query('''
        SELECT %s(value)
        FROM "%s"."%s"."%s" %s
    ''')
        .period(%s)
        .every(%s)
        .groupBy(%s)
        %s
    |alert()
        .crit(lambda: "%s" %s %s %s)
        .post('%s?version=%s')`
		res = fmt.Sprintf(batch, alarm.Func, alarm.DB, alarm.RP, alarm.Measurement, queryWhere, alarm.Period, alarm.Every,
			groupby, offset, alarm.Func, alarm.Expression, alarm.Value, timeLambda, k.EventAddr, alarm.Version)
	default:
		return "", fmt.Errorf("unknown alarm type: %s", models.DeadMan)
	}
	return res, nil
}
