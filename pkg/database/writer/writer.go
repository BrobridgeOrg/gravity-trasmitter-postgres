package writer

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	gravity_sdk_types_record "github.com/BrobridgeOrg/gravity-sdk/types/record"
	"github.com/BrobridgeOrg/gravity-transmitter-postgres/pkg/database"
	buffered_input "github.com/cfsghost/buffered-input"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

var (
	UpdateTemplate = `UPDATE "%s" SET %s WHERE "%s" = :primary_val`
	InsertTemplate = `INSERT INTO "%s" (%s) VALUES (%s)`
	DeleteTemplate = `DELETE FROM "%s" WHERE "%s" = :primary_val`
)

type DatabaseInfo struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Secure   bool   `json:"secure"`
	Username string `json:"username"`
	Password string `json:"password"`
	DbName   string `json:"dbname"`
}

type Writer struct {
	dbInfo            *DatabaseInfo
	db                *sqlx.DB
	commands          chan *DBCommand
	completionHandler database.CompletionHandler
	buffer            *buffered_input.BufferedInput
	tmpQueryStr       string
	handleQueryStr    string
}

func NewWriter() *Writer {
	writer := &Writer{
		dbInfo:            &DatabaseInfo{},
		commands:          make(chan *DBCommand, 2048),
		completionHandler: func(database.DBCommand) {},
		tmpQueryStr:       "",
		handleQueryStr:    "",
	}

	// Initializing buffered input
	opts := buffered_input.NewOptions()
	opts.ChunkSize = 100
	opts.ChunkCount = 10000
	opts.Timeout = 50 * time.Millisecond
	opts.Handler = writer.chunkHandler
	writer.buffer = buffered_input.NewBufferedInput(opts)

	return writer
}

func (writer *Writer) Init() error {

	// Read configuration file
	writer.dbInfo.Host = viper.GetString("database.host")
	writer.dbInfo.Port = viper.GetInt("database.port")
	writer.dbInfo.Secure = viper.GetBool("database.secure")
	writer.dbInfo.Username = viper.GetString("database.username")
	writer.dbInfo.Password = viper.GetString("database.password")
	writer.dbInfo.DbName = viper.GetString("database.dbname")

	log.WithFields(log.Fields{
		"host":     writer.dbInfo.Host,
		"port":     writer.dbInfo.Port,
		"secure":   writer.dbInfo.Secure,
		"username": writer.dbInfo.Username,
		"dbname":   writer.dbInfo.DbName,
	}).Info("Connecting to database")

	sslmode := "disable"
	if writer.dbInfo.Secure {
		sslmode = "enable"
	}

	connStr := fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s",
		writer.dbInfo.Username,
		writer.dbInfo.Password,
		writer.dbInfo.Host,
		writer.dbInfo.Port,
		writer.dbInfo.DbName,
		sslmode,
	)

	// Open database
	db, err := sqlx.Open("postgres", connStr)
	if err != nil {
		log.Error(err)
		return err
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(10)

	writer.db = db

	go writer.run()

	return nil
}

func (writer *Writer) chunkHandler(chunk []interface{}) {

	dbCommands := make([]*DBCommand, 0, len(chunk))
	for _, request := range chunk {
		req := request.(*DBCommand)
		dbCommands = append(dbCommands, req)
	}
	writer.processData(dbCommands)
}

func (writer *Writer) processData(dbCommands []*DBCommand) {
	// Write to Database
	for {
	LOOP:

		tx, err := writer.db.Beginx()
		if err != nil {
			log.Error(err)
			tx.Rollback()

			<-time.After(time.Second * 5)

			log.WithFields(log.Fields{}).Warn("Retry to write record to database by batch ...")
			continue
		}

		for _, cmd := range dbCommands {
			_, err := tx.NamedExec(cmd.QueryStr, cmd.Args)
			if err != nil {
				log.WithFields(log.Fields{
					"pkey_field": cmd.Record.PrimaryKey,
				}).Error(err)
				log.Error(cmd.QueryStr)
				log.Error(cmd.Args)
				tx.Rollback()
				<-time.After(time.Second * 5)
				goto LOOP

			}
		}
		err = tx.Commit()

		if err != nil {
			log.Error(err)
			tx.Rollback()

			<-time.After(time.Second * 5)

			log.WithFields(log.Fields{}).Warn("Retry to write record to database by batch ...")
			continue
		}

		break
	}

	for _, cmd := range dbCommands {
		writer.completionHandler(database.DBCommand(cmd))
	}
}

func (writer *Writer) run() {
	for {
		select {
		case cmd := <-writer.commands:
			// publish to buffered-input
			writer.buffer.Push(cmd)
		}
	}
}

func (writer *Writer) SetCompletionHandler(fn database.CompletionHandler) {
	writer.completionHandler = fn
}

func (writer *Writer) ProcessData(reference interface{}, record *gravity_sdk_types_record.Record) error {
	/*
		log.WithFields(log.Fields{
			"method": record.Method,
			"event":  record.EventName,
			"table":  record.Table,
		}).Info("Write record")
	*/
	switch record.Method {
	case gravity_sdk_types_record.Method_DELETE:
		return writer.DeleteRecord(reference, record)
	case gravity_sdk_types_record.Method_UPDATE:
		return writer.UpdateRecord(reference, record)
	case gravity_sdk_types_record.Method_INSERT:
		return writer.InsertRecord(reference, record)
	}

	return nil
}

func (writer *Writer) GetDefinition(record *gravity_sdk_types_record.Record) (*gravity_sdk_types_record.RecordDef, error) {

	recordDef := &gravity_sdk_types_record.RecordDef{
		HasPrimary: false,
		Values:     make(map[string]interface{}),
		ColumnDefs: make([]*gravity_sdk_types_record.ColumnDef, 0, len(record.Fields)),
	}

	// Scanning fields
	for n, field := range record.Fields {

		value := gravity_sdk_types_record.GetValue(field.Value)

		// Primary key
		//		if field.IsPrimary == true {
		if record.PrimaryKey == field.Name {
			recordDef.Values["primary_val"] = value
			recordDef.HasPrimary = true
			recordDef.PrimaryColumn = field.Name
			continue
		}

		// Generate binding name
		bindingName := fmt.Sprintf("val_%s", strconv.Itoa(n))
		recordDef.Values[bindingName] = value

		// Store definition
		recordDef.ColumnDefs = append(recordDef.ColumnDefs, &gravity_sdk_types_record.ColumnDef{
			ColumnName:  field.Name,
			Value:       field.Name,
			BindingName: bindingName,
		})
	}

	if len(record.PrimaryKey) > 0 && !recordDef.HasPrimary {
		log.WithFields(log.Fields{
			"column": record.PrimaryKey,
		}).Error("Not found primary key")

		return nil, errors.New("Not found primary key")
	}

	return recordDef, nil
}

func (writer *Writer) InsertRecord(reference interface{}, record *gravity_sdk_types_record.Record) error {

	recordDef, err := writer.GetDefinition(record)
	if err != nil {
		return err
	}

	return writer.insert(reference, record, record.Table, recordDef)
}

func (writer *Writer) UpdateRecord(reference interface{}, record *gravity_sdk_types_record.Record) error {

	recordDef, err := writer.GetDefinition(record)
	if err != nil {
		return err
	}

	// Ignore if no primary key
	if recordDef.HasPrimary == false {
		return nil
	}

	_, err = writer.update(reference, record, record.Table, recordDef)
	if err != nil {
		return err
	}

	return nil
}

func (writer *Writer) DeleteRecord(reference interface{}, record *gravity_sdk_types_record.Record) error {

	if record.PrimaryKey == "" {
		// Do nothing
		return nil
	}

	for _, field := range record.Fields {

		// Primary key
		//		if field.IsPrimary == true {
		if record.PrimaryKey == field.Name {

			value := gravity_sdk_types_record.GetValue(field.Value)

			sqlStr := fmt.Sprintf(DeleteTemplate, record.Table, field.Name)

			writer.commands <- &DBCommand{
				Reference: reference,
				Record:    record,
				QueryStr:  sqlStr,
				Args: map[string]interface{}{
					"primary_val": value,
				},
			}

			break
		}
	}

	return nil
}

func (writer *Writer) update(reference interface{}, record *gravity_sdk_types_record.Record, table string, recordDef *gravity_sdk_types_record.RecordDef) (bool, error) {

	// Preparing SQL string
	updates := make([]string, 0, len(recordDef.ColumnDefs))
	for _, def := range recordDef.ColumnDefs {
		updates = append(updates, `"`+def.ColumnName+`" = :`+def.BindingName)
	}

	updateStr := strings.Join(updates, ",")
	sqlStr := fmt.Sprintf(UpdateTemplate, table, updateStr, recordDef.PrimaryColumn)

	writer.commands <- &DBCommand{
		Reference: reference,
		Record:    record,
		QueryStr:  sqlStr,
		Args:      recordDef.Values,
	}

	return false, nil
}

func (writer *Writer) insert(reference interface{}, record *gravity_sdk_types_record.Record, table string, recordDef *gravity_sdk_types_record.RecordDef) error {

	paramLength := len(recordDef.ColumnDefs)
	if recordDef.HasPrimary {
		paramLength++
	}

	// Allocation
	colNames := make([]string, 0, paramLength)
	valNames := make([]string, 0, paramLength)

	if recordDef.HasPrimary {
		colNames = append(colNames, `"`+recordDef.PrimaryColumn+`"`)
		valNames = append(valNames, ":primary_val")
	}

	// Preparing columns and bindings
	for _, def := range recordDef.ColumnDefs {
		colNames = append(colNames, `"`+def.ColumnName+`"`)
		valNames = append(valNames, `:`+def.BindingName)
	}

	// Preparing SQL string to insert
	colsStr := strings.Join(colNames, ",")
	valsStr := strings.Join(valNames, ",")
	insertStr := fmt.Sprintf(InsertTemplate, table, colsStr, valsStr)

	//	database.db.NamedExec(insertStr, recordDef.Values)
	writer.commands <- &DBCommand{
		Reference: reference,
		Record:    record,
		QueryStr:  insertStr,
		Args:      recordDef.Values,
	}

	return nil
}
