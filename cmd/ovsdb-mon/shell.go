package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"reflect"
	"strings"
	"sync"
	"time"

	ishell "github.com/abiosoft/ishell/v2"
	"github.com/kylelemons/godebug/diff"
	"github.com/kylelemons/godebug/pretty"
	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
)

type eventType string

const (
	updateEvent eventType = "UPDATE"
	addEvent    eventType = "ADD"
	deleteEvent eventType = "DELETE"

	sname = "ovsdbShell"
)

type OvsdbEvent struct {
	Timestamp time.Time
	Event     eventType
	Table     string
	Old       model.Model
	New       model.Model
}

type OvsdbShell struct {
	mutex           *sync.RWMutex
	monitor         bool
	ovs             *client.Client
	dbModel         *model.DBModel
	events          []OvsdbEvent
	tablesToMonitor []client.TableMonitor
}

func (s *OvsdbShell) Monitor(monitor bool) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.monitor = monitor
}

func (s *OvsdbShell) printEvent(event OvsdbEvent) {
	fmt.Printf("New \033[1m%s\033[0m event on table: \033[1m%s\033[0m\n", event.Event, event.Table)
	switch event.Event {
	case updateEvent:
		fmt.Println(colordiff(event.Old, event.New))
	case addEvent:
		fmt.Printf("\x1b[32m%s\x1b[0m\n", pretty.CompareConfig.Sprint(event.New))
	case deleteEvent:
		fmt.Printf("\x1b[31m%s\x1b[0m\n", pretty.CompareConfig.Sprint(event.Old))
	}
	fmt.Print("\n")
}

func (s *OvsdbShell) OnAdd(table string, m model.Model) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	if s.monitor {
		event := OvsdbEvent{
			Timestamp: time.Now(),
			Event:     addEvent,
			Table:     table,
			New:       m,
		}
		s.printEvent(event)
		s.events = append(s.events, event)
	}
}

func (s *OvsdbShell) OnUpdate(table string, old, new model.Model) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	if s.monitor {
		event := OvsdbEvent{
			Timestamp: time.Now(),
			Event:     updateEvent,
			Table:     table,
			New:       new,
			Old:       old,
		}
		s.printEvent(event)
		s.events = append(s.events, event)
	}
}

func (s *OvsdbShell) OnDelete(table string, m model.Model) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	if s.monitor {
		event := OvsdbEvent{
			Timestamp: time.Now(),
			Event:     deleteEvent,
			Table:     table,
			Old:       m,
		}
		s.printEvent(event)
		s.events = append(s.events, event)
	}
}

func (s *OvsdbShell) Save(filePath string) error {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	content, err := json.MarshalIndent(s.events, "", "    ")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(filePath, content, 0644)
}

func (s *OvsdbShell) Run(ovsPtr *client.Client, args ...string) {
	s.ovs = ovsPtr
	if ovsPtr == nil {
		panic("Failed to de-reference ovs client")
	}
	ovs := *ovsPtr
	ovs.Cache().AddEventHandler(s)

	// if _, err := ovs.MonitorAll(context.Background()); err != nil {
	if _, err := ovs.Monitor(context.Background(), s.tablesToMonitor...); err != nil {
		panic(err)
	}

	shell := ishell.New()
	if shell == nil {
		panic("Failed to create shell")
	}
	shell.Set(sname, s)

	shell.Println("OVSDB Monitoring Shell")
	shell.AddCmd(&ishell.Cmd{
		Name: "start",
		Help: "Start monitoring activity of the OVSDB DB",
		Func: func(c *ishell.Context) {
			ovsdbShell := c.Get(sname)
			if ovsdbShell == nil {
				c.Println("Error: No context")
			}
			ovsdbShell.(*OvsdbShell).Monitor(true)
		},
	})
	shell.AddCmd(&ishell.Cmd{
		Name: "stop",
		Help: "Stop monitoring activity of the OVSDB DB",
		Func: func(c *ishell.Context) {
			ovsdbShell := c.Get(sname)
			if ovsdbShell == nil {
				c.Println("Error: No context")
			}
			ovsdbShell.(*OvsdbShell).Monitor(false)
		},
	})
	shell.AddCmd(&ishell.Cmd{
		Name: "save",
		Help: "Save events",
		Func: func(c *ishell.Context) {
			ovsdbShell := c.Get(sname)
			if len(c.Args) != 1 {
				c.Println("Usage: save <path>")
				return
			}
			filePath := c.Args[0]
			if err := ovsdbShell.(*OvsdbShell).Save(filePath); err != nil {
				c.Println(err)
			} else {
				c.Println("File saved")
			}

		},
	})

	shell.AddCmd(&ishell.Cmd{
		Name: "show",
		Help: "Print available tables",
		Func: func(c *ishell.Context) {
			ovsdbShell := c.Get(sname)
			if ovsdbShell == nil {
				c.Println("Error: No context")
			}
			c.Println("Available Tables")
			c.Println("----------------")

			ovsPtr := ovsdbShell.(*OvsdbShell).ovs
			if ovsPtr != nil {
				for name := range (*ovsPtr).Schema().Tables {
					c.Println(name)
				}
			} else {
				c.Println("None: no ovs client")
			}
		},
	})

	// List Command
	// Add a subcommand per table
	listCmd := ishell.Cmd{
		Name: "list",
		Help: "List the content of a specific table",
	}

	// Generate the list of columns for each table to be used as command auto-completion options
	exactTableFields := make(map[string][]string) // holds exact names
	allTableFields := make(map[string][]string)   // holds exact names and lower case versions
	for tname, mtype := range s.dbModel.Types() {
		exactFields := []string{}
		allFields := []string{}
		for i := 0; i < mtype.Elem().NumField(); i++ {
			exactFields = append(exactFields, mtype.Elem().Field(i).Name)
			allFields = append(allFields, mtype.Elem().Field(i).Name)
			allFields = append(allFields, strings.ToLower(mtype.Elem().Field(i).Name))
		}
		allTableFields[tname] = allFields
		exactTableFields[tname] = exactFields
	}

	for name := range s.dbModel.Types() {
		// Trick to be able to use name inside closures
		tableName := name
		subTableCmd := ishell.Cmd{
			Name:    name,
			Aliases: []string{strings.ToLower(name)},
			Help:    fmt.Sprintf("%s [Field1 Field2 ...]", name),
			LongHelp: fmt.Sprintf(
				"List the content of Table %s", name) +
				fmt.Sprintf("\n\n%s [--filter Field=Value] [Field1 Field2 ...]", name) +
				"\n\t[Field1 Field2 ...]: List of fields to show (default: all fields will be shown)" +
				"\n\t[--filter Field=Value]: Apply filter (only fields that are part of the table's index can be specified" +
				fmt.Sprintf("\n\t\tPossible Fields: %s", strings.Join(exactTableFields[name], ", ")),
			Func: func(c *ishell.Context) {
				ovsdbShell := c.Get(sname)
				if ovsdbShell == nil {
					c.Println("Error: No context")
				}

				columns := []string{}
				var filter string
				var err error
				isFilter := false
				for _, arg := range c.Args {
					if arg == "--filter" {
						isFilter = true
					} else {
						if isFilter {
							if filter != "" {
								c.Println("Only one --filter statement allowed")
								return
							}
							filter = arg
							isFilter = false
						} else {
							columns = append(columns, arg)
						}
					}
				}
				// Use a buffer to store the generated output table
				buffer := bytes.Buffer{}
				mtype := ovsdbShell.(*OvsdbShell).dbModel.Types()[c.Cmd.Name]
				printer, err := NewStructPrinter(&buffer, mtype.Elem(), columns...)
				if err != nil {
					c.Println(err)
					return
				}

				valueList := reflect.New(reflect.SliceOf(mtype.Elem()))
				ovsPtr := ovsdbShell.(*OvsdbShell).ovs
				if ovsPtr == nil {
					c.Println("No ovs client")
					return
				}
				if filter != "" {
					cond, err := s.filterAPI(tableName, filter)
					if err != nil {
						c.Println(err)
						return
					}
					err = cond.List(valueList.Interface())
					if err != nil && err != client.ErrNotFound {
						c.Println(err)
						return
					}
				} else {
					err = (*ovsPtr).List(valueList.Interface())
					if err != nil && err != client.ErrNotFound {
						c.Println(err)
						return
					}
				}

				// Render the result table
				err = printer.Append(reflect.Indirect(valueList).Interface())
				if err != nil {
					c.Println(err)
				}
				printer.Render()
				// Print the result table through shell so it can be paged
				if err := c.ShowPaged(buffer.String()); err != nil {
					panic(err)
				}
			},
			CompleterWithPrefix: func(prefix string, args []string) []string {
				if prefix == "" {
					return exactTableFields[tableName]
				}
				return allTableFields[tableName]
			},
		}
		listCmd.AddCmd(&subTableCmd)
	}

	// The list command autocompleter returns the exact table names if user has not
	// started typing. If he/she has, then also include aliases (which are lower cased versions
	// of the table names)
	listCmd.CompleterWithPrefix = func(prefix string, args []string) []string {
		options := []string{}
		for _, cmd := range listCmd.Children() {
			options = append(options, cmd.Name)
			if prefix != "" {
				options = append(options, cmd.Aliases...)
			}
		}
		return options
	}
	shell.AddCmd(&listCmd)

	// If we have arguments, just run them and exit
	if len(args) > 0 {
		if err := shell.Process(args...); err != nil {
			panic(err)
		}
	} else {
		shell.Run()
	}
}

func newOvsdbShell(auto bool, dbmodel *model.DBModel, tablesToMonitor []client.TableMonitor) *OvsdbShell {
	return &OvsdbShell{
		mutex:           new(sync.RWMutex),
		monitor:         auto,
		dbModel:         dbmodel,
		tablesToMonitor: tablesToMonitor,
	}
}

// colordiff is similar to what pretty.compare does but with colors
func colordiff(a, b interface{}) string {
	config := pretty.CompareConfig
	alines := strings.Split(config.Sprint(a), "\n")
	blines := strings.Split(config.Sprint(b), "\n")

	buf := new(strings.Builder)
	for _, c := range diff.DiffChunks(alines, blines) {
		for _, line := range c.Added {
			fmt.Fprintf(buf, "\x1b[32m+%s\x1b[0m\n", line)
		}
		for _, line := range c.Deleted {
			fmt.Fprintf(buf, "\x1b[31m-%s\x1b[0m\n", line)
		}
		for _, line := range c.Equal {
			fmt.Fprintf(buf, " %s\n", line)
		}
	}
	return strings.TrimRight(buf.String(), "\n")
}

// filterAPI returns the conditional API that filters based on the provided filter expression
// Expression is [FIELD]=[VALUE]
func (s *OvsdbShell) filterAPI(tableName string, expr string) (client.ConditionalAPI, error) {
	condModel, err := s.dbModel.NewModel(tableName)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(expr, "=")
	if len(parts) != 2 {
		return nil, fmt.Errorf("Invalid filter expression: %s", expr)
	}

	field := parts[0]
	value := parts[1]

	fieldVal := reflect.ValueOf(condModel).Elem().FieldByName(field)
	if !fieldVal.IsValid() {
		return nil, fmt.Errorf("field %s not present in database table %s", field, tableName)
	}
	if fieldVal.Kind() != reflect.String {
		return nil, fmt.Errorf("filters only support string values")
	}

	fieldVal.Set(reflect.ValueOf(value))

	return (*s.ovs).Where(condModel), nil
}
