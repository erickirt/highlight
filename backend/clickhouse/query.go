package clickhouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/highlight-run/highlight/backend/model"
	"github.com/highlight-run/highlight/backend/parser"
	"github.com/highlight-run/highlight/backend/parser/listener"
	modelInputs "github.com/highlight-run/highlight/backend/private-graph/graph/model"
	"github.com/highlight-run/highlight/backend/util"
	"github.com/highlight/highlight/sdk/highlight-go"
	"github.com/huandu/go-sqlbuilder"
	"github.com/nqd/flat"
	e "github.com/pkg/errors"
	"github.com/samber/lo"
	"go.openly.dev/pointy"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"

	sqlparser "github.com/highlight/clickhouse-sql-parser/parser"
)

const SamplingRows = 20_000_000
const KeysMaxRows = 1_000_000
const KeyValuesMaxRows = 1_000_000
const AllKeyValuesMaxRows = 100_000_000
const MaxBuckets = 240
const NoLimit = 1_000_000_000_000

const bucketIndexAlias = "__highlight_bucket_index"
const minAlias = "__highlight_min"
const maxAlias = "__highlight_max"
const projectIdSetting = "SQL_highlight_project_id"

type SampleableTableConfig struct {
	tableConfig         model.TableConfig
	samplingTableConfig model.TableConfig
	sampleSizeRows      uint64
}

type ReadMetricsInput struct {
	SampleableConfig   SampleableTableConfig
	ProjectIDs         []int
	Sql                *string
	Params             modelInputs.QueryInput
	GroupBy            []string
	BucketCount        *int
	BucketWindow       *int
	BucketBy           string
	Limit              *int
	LimitAggregator    *modelInputs.MetricAggregator
	LimitColumn        *string
	SavedMetricState   *SavedMetricState
	PredictionSettings *modelInputs.PredictionSettings
	NoBucketMax        bool
	Expressions        []*modelInputs.MetricExpressionInput
}

func readObjects[TObj interface{}](ctx context.Context, client *Client, config model.TableConfig, samplingConfig model.TableConfig, projectID int, params modelInputs.QueryInput, pagination Pagination, scanObject func(driver.Rows) (*Edge[TObj], error)) (*Connection[TObj], error) {
	limit := LogsLimit
	if pagination.Limit != nil {
		limit = *pagination.Limit
	}

	sb := sqlbuilder.NewSelectBuilder()
	var err error
	var args []interface{}

	innerTableConfig := config
	if useSamplingTable(params) {
		innerTableConfig = samplingConfig
	}

	orderForward, _ := getSortOrders(sb, pagination.Direction, config, params)

	outerSelect := strings.Join(config.SelectColumns, ", ")
	innerSelect := []string{"Timestamp", "UUID"}
	if pagination.At != nil && len(*pagination.At) > 1 {
		// Create a "window" around the cursor
		// https://stackoverflow.com/a/71738696
		beforeSb, _, err := makeSelectBuilder(
			innerTableConfig,
			innerSelect,
			[]int{projectID},
			params,
			Pagination{
				Before: pagination.At,
			})
		if err != nil {
			return nil, err
		}
		beforeSb.Distinct().Limit(limit/2 + 1)

		atSb, _, err := makeSelectBuilder(
			innerTableConfig,
			innerSelect,
			[]int{projectID},
			params,
			Pagination{
				At: pagination.At,
			})
		if err != nil {
			return nil, err
		}
		atSb.Distinct()

		afterSb, _, err := makeSelectBuilder(
			innerTableConfig,
			innerSelect,
			[]int{projectID},
			params,
			Pagination{
				After: pagination.At,
			})
		if err != nil {
			return nil, err
		}
		afterSb.Distinct().Limit(limit/2 + 1)

		ub := sqlbuilder.UnionAll(beforeSb, atSb, afterSb)
		sb.Select(outerSelect).
			Distinct().
			From(config.TableName).
			Where(sb.Equal("ProjectId", projectID)).
			Where(sb.In(fmt.Sprintf("(%s)", strings.Join(innerSelect, ",")), ub)).
			OrderBy(orderForward)
	} else {
		fromSb, _, err := makeSelectBuilder(
			innerTableConfig,
			innerSelect,
			[]int{projectID},
			params,
			pagination)
		if err != nil {
			return nil, err
		}

		fromSb.Distinct().Limit(limit + 1)
		sb.Select(outerSelect).
			Distinct().
			From(config.TableName).
			Where(sb.Equal("ProjectId", projectID)).
			Where(sb.In(fmt.Sprintf("(%s)", strings.Join(innerSelect, ",")), fromSb)).
			OrderBy(orderForward).
			Limit(limit + 1)
	}

	sql, args := sb.BuildWithFlavor(sqlbuilder.ClickHouse)

	span, _ := util.StartSpanFromContext(ctx, "clickhouse.Query")
	span.SetAttribute(string(semconv.DBNamespaceKey), innerTableConfig.TableName)
	span.SetAttribute(string(semconv.DBQueryTextKey), sql)
	span.SetAttribute(string(semconv.DBSystemKey), "clickhouse")
	span.SetAttribute(string(semconv.DBOperationNameKey), "SELECT")
	span.SetAttribute(string("db.query.summary"), "SELECT "+strings.Join(config.SelectColumns, ", ")+" FROM "+innerTableConfig.TableName)
	span.SetAttribute("db.operation.parameters", args)

	rows, err := client.conn.Query(ctx, sql, args...)

	if err != nil {
		span.Finish(err)
		return nil, err
	}

	edges := []*Edge[TObj]{}

	for rows.Next() {
		edge, err := scanObject(rows)
		if err != nil {
			return nil, err
		}

		edges = append(edges, edge)
	}
	rows.Close()

	span.Finish(rows.Err())
	return getConnection(edges, pagination), nil
}

func transformSql(
	config model.TableConfig,
	input ReadMetricsInput,
) (string, error) {
	if input.Sql == nil {
		return "", e.Errorf("Sql input cannot be nil")
	}

	parser := sqlparser.NewParser(*input.Sql)
	statements, err := parser.ParseStmts()
	if err != nil {
		return "", err
	}

	if len(statements) != 1 {
		return "", e.Errorf("Expected 1 SQL statement, found %d", len(statements))
	}
	expr := statements[0]

	settingsVisitor := sqlparser.DefaultASTVisitor{}
	settingsVisitor.Visit = func(expr sqlparser.Expr) error {
		_, found := expr.(*sqlparser.SettingsClause)
		if found {
			return e.New("SQL statement cannot include a settings clause.")
		}
		return nil
	}
	if err := expr.Accept(&settingsVisitor); err != nil {
		return "", err
	}

	priorVisit := map[sqlparser.Expr]bool{}

	selectVisitor := sqlparser.DefaultASTVisitor{}
	selectVisitor.Visit = func(expr sqlparser.Expr) error {
		selectQuery, ok := expr.(*sqlparser.SelectQuery)
		if !ok {
			return nil
		}

		isResourceTable := false
		if selectQuery.From != nil {
			_, ok := selectQuery.From.Expr.(*sqlparser.JoinExpr)
			if ok {
				resourceTableCheckVisitor := sqlparser.DefaultASTVisitor{}
				resourceTableCheckVisitor.Visit = func(expr sqlparser.Expr) error {
					if priorVisit[expr] {
						return nil
					}
					switch typed := expr.(type) {
					case *sqlparser.TableIdentifier:
						if resourceTables[typed.Table.Name] {
							return e.New("Resource tables cannot be used in JOIN expression")
						}
					}
					return nil
				}

				if err := selectQuery.From.Expr.Accept(&resourceTableCheckVisitor); err != nil {
					return err
				}
			} else {
				fromExpr, ok := selectQuery.From.Expr.(*sqlparser.JoinTableExpr)
				if !ok || fromExpr == nil || fromExpr.Table == nil {
					return e.New("Expected FROM expression to be JoinTableExpr")
				}

				tableIdentifier, ok := fromExpr.Table.Expr.(*sqlparser.TableIdentifier)
				if ok && tableIdentifier != nil && tableIdentifier.Table != nil && resourceTables[tableIdentifier.Table.Name] {
					isResourceTable = true
				}
			}
		}

		// Ignore function, alias, and table names
		ignoreList := map[sqlparser.Expr]bool{}
		aliases := map[string]bool{}
		ignoreVisitor := sqlparser.DefaultASTVisitor{}
		ignoreVisitor.Visit = func(expr sqlparser.Expr) error {
			if priorVisit[expr] {
				return nil
			}
			switch typed := expr.(type) {
			case *sqlparser.SelectItem:
				ignoreList[typed.Alias] = true
				if typed.Alias != nil {
					aliases[typed.Alias.Name] = true
				}
			case *sqlparser.FunctionExpr:
				ignoreList[typed.Name] = true
			case *sqlparser.AliasExpr:
				ignoreList[typed.Alias] = true
				if typed.Alias != nil {
					aliases[typed.Alias.String()] = true
				}
			case *sqlparser.TableIdentifier:
				ignoreList[typed.Table] = true
			case *sqlparser.IntervalExpr:
				ignoreList[typed.Unit] = true
			}

			return nil
		}

		fields := []string{}
		columnReplacementVisitor := sqlparser.DefaultASTVisitor{}
		columnReplacementVisitor.Visit = func(expr sqlparser.Expr) error {
			if priorVisit[expr] {
				return nil
			}
			switch typed := expr.(type) {
			case *sqlparser.Ident:
				if typed.Name == "*" || ignoreList[expr] || aliases[typed.Name] {
					break
				}
				if col, found := config.KeysToColumns[typed.Name]; found {
					typed.Name = col
				} else {
					fields = append(fields, typed.Name)
					typed.Name = fmt.Sprintf("%s['%s']", model.GetAttributesColumn(config.AttributesColumns, typed.Name), typed.Name)
					typed.QuoteType = 0
				}
			}
			return nil
		}

		functionReplacementVisitor := sqlparser.DefaultASTVisitor{}
		functionReplacementVisitor.Visit = func(expr sqlparser.Expr) error {
			if priorVisit[expr] {
				return nil
			}
			switch typed := expr.(type) {
			case *sqlparser.FunctionExpr:
				if typed.Name != nil && typed.Name.Name == "$time_interval" {
					// Replace toStartOfInterval with the relevant $time_interval
					typed.Name.Name = "toStartOfInterval"
					if typed.Params.Items == nil {
						return e.New("$time_interval called with empty argument list")
					}

					if len(typed.Params.Items.Items) != 1 {
						return e.Errorf("Expecting 1 argument for $time_interval, found %d", len(typed.Params.Items.Items))
					}

					columnExpr, ok := typed.Params.Items.Items[0].(*sqlparser.ColumnExpr)
					if !ok {
						return e.Errorf("Expecting $time_interval argument to be a string literal.")
					}
					stringLiteral, ok := columnExpr.Expr.(*sqlparser.StringLiteral)
					if !ok {
						return e.Errorf("Expecting $time_interval argument to be a string literal.")
					}

					typed.Params.Items.Items = []sqlparser.Expr{
						&sqlparser.ColumnExpr{
							Expr: &sqlparser.Ident{Name: "Timestamp"},
						},
						&sqlparser.IntervalExpr{
							Expr: stringLiteral,
							Unit: &sqlparser.Ident{},
						},
					}
				} else if typed.Name != nil {
					// Apply _sample_factor to count or sum functions
					isSample := strings.Contains(strings.ToLower(config.TableName), "sample")
					funcLower := strings.ToLower(typed.Name.Name)
					if isSample && (strings.Contains(funcLower, "sum") || strings.Contains(funcLower, "count")) {
						typed.Name.Name = fmt.Sprintf("any(_sample_factor) * %s", typed.Name.Name)
					}
				}
			}
			return nil
		}

		tableReplacementVisitor := sqlparser.DefaultASTVisitor{}
		tableReplacementVisitor.Visit = func(expr sqlparser.Expr) error {
			if priorVisit[expr] {
				return nil
			}
			switch typed := expr.(type) {
			case *sqlparser.TableIdentifier:
				typed.Table.Name = config.TableName
			}
			return nil
		}

		hasTimestamp := false
		timestampVisitor := sqlparser.DefaultASTVisitor{}
		timestampVisitor.Visit = func(expr sqlparser.Expr) error {
			if priorVisit[expr] {
				return nil
			}
			switch typed := expr.(type) {
			case *sqlparser.Ident:
				if typed.Name == "Timestamp" {
					hasTimestamp = true
				}
			}
			return nil
		}

		priorVisitVisitor := sqlparser.DefaultASTVisitor{}
		priorVisitVisitor.Visit = func(expr sqlparser.Expr) error {
			priorVisit[expr] = true
			return nil
		}

		if isResourceTable {
			if err := selectQuery.Accept(&ignoreVisitor); err != nil {
				return err
			}
			if err := selectQuery.Accept(&columnReplacementVisitor); err != nil {
				return err
			}
			if err := selectQuery.Accept(&functionReplacementVisitor); err != nil {
				return err
			}
			if err := selectQuery.Accept(&tableReplacementVisitor); err != nil {
				return err
			}

			if len(input.ProjectIDs) != 1 {
				return e.Errorf("SQL queries must use 1 project id, %d found", len(input.ProjectIDs))
			}
			projectId := input.ProjectIDs[0]

			projectIdCol := "ProjectId"
			if config.TableName == "sessions" || config.TableName == "errors" {
				projectIdCol = "ProjectID"
			}

			whereExpr := &sqlparser.BinaryOperation{
				LeftExpr: &sqlparser.Ident{
					Name: projectIdCol,
				},
				RightExpr: &sqlparser.NumberLiteral{
					Literal: strconv.Itoa(projectId),
				},
				Operation: "=",
			}

			addAttributesParser(config, fields, projectId, input.Params, selectQuery)

			timestampCheck := &sqlparser.BinaryOperation{
				LeftExpr: &sqlparser.BinaryOperation{
					LeftExpr: &sqlparser.Ident{
						Name: "Timestamp",
					},
					RightExpr: &sqlparser.FunctionExpr{
						Name: &sqlparser.Ident{
							Name: "toDateTime",
						},
						Params: &sqlparser.ParamExprList{
							Items: &sqlparser.ColumnExprList{
								Items: []sqlparser.Expr{&sqlparser.NumberLiteral{
									Literal: strconv.FormatInt(input.Params.DateRange.StartDate.Unix(), 10),
								}},
							},
						},
					},
					Operation: ">=",
				},
				RightExpr: &sqlparser.BinaryOperation{
					LeftExpr: &sqlparser.Ident{
						Name: "Timestamp",
					},
					RightExpr: &sqlparser.FunctionExpr{
						Name: &sqlparser.Ident{
							Name: "toDateTime",
						},
						Params: &sqlparser.ParamExprList{
							Items: &sqlparser.ColumnExprList{
								Items: []sqlparser.Expr{&sqlparser.NumberLiteral{
									Literal: strconv.FormatInt(input.Params.DateRange.EndDate.Unix(), 10),
								}},
							},
						},
					},
					Operation: "<=",
				},
				Operation: "AND",
			}

			if selectQuery.Where == nil {
				whereExpr = &sqlparser.BinaryOperation{
					LeftExpr:  whereExpr,
					RightExpr: timestampCheck,
					Operation: "AND",
				}
				selectQuery.Where = &sqlparser.WhereClause{
					Expr: whereExpr,
				}
			} else {
				// If the WHERE clause does not use timestamp, add the dashboard's start/end dates.
				if err := selectQuery.Where.Accept(&timestampVisitor); err != nil {
					return err
				}
				if !hasTimestamp {
					whereExpr = &sqlparser.BinaryOperation{
						LeftExpr:  whereExpr,
						RightExpr: timestampCheck,
						Operation: "AND",
					}
				}

				copiedClause := selectQuery.Where.Expr
				selectQuery.Where.Expr = &sqlparser.BinaryOperation{
					LeftExpr: &sqlparser.ParamExprList{
						Items: &sqlparser.ColumnExprList{
							Items: []sqlparser.Expr{copiedClause},
						}},
					Operation: "AND",
					RightExpr: whereExpr,
				}
			}
		}

		if err := selectQuery.Accept(&priorVisitVisitor); err != nil {
			return err
		}

		return nil
	}

	if err := expr.Accept(&selectVisitor); err != nil {
		return "", err
	}

	sql := expr.String()

	return sql, nil
}

var resourceTables = map[string]bool{
	"sessions": true,
	"errors":   true,
	"logs":     true,
	"traces":   true,
	"events":   true,
	"metrics":  true,
}

func GetTables(sql string) ([]string, error) {
	parser := sqlparser.NewParser(sql)
	statements, err := parser.ParseStmts()
	if err != nil {
		return nil, err
	}

	if len(statements) != 1 {
		return nil, e.Errorf("Expected 1 SQL statement, found %d", len(statements))
	}
	stmt := statements[0]

	tables := []string{}
	visitor := sqlparser.DefaultASTVisitor{}
	visitor.Visit = func(expr sqlparser.Expr) error {
		switch typed := expr.(type) {
		case *sqlparser.TableIdentifier:
			if typed.Table != nil && resourceTables[typed.Table.Name] {
				tables = append(tables, typed.Table.Name)
			}
		}
		return nil
	}

	if err := stmt.Accept(&visitor); err != nil {
		return nil, err
	}

	return lo.Uniq(tables), nil
}

func makeSelectBuilder(
	config model.TableConfig,
	selectCols []string,
	projectIDs []int,
	params modelInputs.QueryInput,
	pagination Pagination,
) (*sqlbuilder.SelectBuilder, listener.Filters, error) {
	sb := sqlbuilder.NewSelectBuilder()

	orderForward, orderBackward := getSortOrders(sb, pagination.Direction, config, params)

	sb.Select(selectCols...)
	sb.From(config.TableName)

	if len(projectIDs) == 1 {
		sb.Where(sb.Equal("ProjectId", projectIDs[0]))
	} else {
		sb.Where(sb.In("ProjectId", projectIDs))
	}

	if pagination.After != nil && len(*pagination.After) > 1 {
		timestamp, uuid, err := decodeCursor(*pagination.After)
		if err != nil {
			return nil, nil, err
		}

		// See https://dba.stackexchange.com/a/206811
		if pagination.Direction == modelInputs.SortDirectionAsc {
			sb.Where(sb.GreaterEqualThan("Timestamp", timestamp)).
				Where(sb.LessEqualThan("Timestamp", params.DateRange.EndDate)).
				Where(
					sb.Or(
						sb.GreaterThan("Timestamp", timestamp),
						sb.GreaterThan("UUID", uuid),
					),
				)
		} else {
			sb.Where(sb.LessEqualThan("Timestamp", timestamp)).
				Where(sb.GreaterEqualThan("Timestamp", params.DateRange.StartDate)).
				Where(
					sb.Or(
						sb.LessThan("Timestamp", timestamp),
						sb.LessThan("UUID", uuid),
					),
				)
		}

		sb.OrderBy(orderForward)
	} else if pagination.At != nil && len(*pagination.At) > 1 {
		timestamp, uuid, err := decodeCursor(*pagination.At)
		if err != nil {
			return nil, nil, err
		}
		sb.Where(sb.Equal("Timestamp", timestamp)).
			Where(sb.Equal("UUID", uuid))
	} else if pagination.Before != nil && len(*pagination.Before) > 1 {
		timestamp, uuid, err := decodeCursor(*pagination.Before)
		if err != nil {
			return nil, nil, err
		}

		// See https://dba.stackexchange.com/a/206811
		if pagination.Direction == modelInputs.SortDirectionAsc {
			sb.Where(sb.LessEqualThan("Timestamp", timestamp)).
				Where(sb.GreaterEqualThan("Timestamp", params.DateRange.StartDate)).
				Where(
					sb.Or(
						sb.LessThan("Timestamp", timestamp),
						sb.LessThan("UUID", uuid),
					),
				)
		} else {
			sb.Where(sb.GreaterEqualThan("Timestamp", timestamp)).
				Where(sb.LessEqualThan("Timestamp", params.DateRange.EndDate)).
				Where(
					sb.Or(
						sb.GreaterThan("Timestamp", timestamp),
						sb.GreaterThan("UUID", uuid),
					),
				)
		}

		sb.OrderBy(orderBackward)
	} else {
		sb.Where(sb.LessEqualThan("Timestamp", params.DateRange.EndDate)).
			Where(sb.GreaterEqualThan("Timestamp", params.DateRange.StartDate))

		if !pagination.CountOnly { // count queries can't be ordered because we don't include Timestamp in the select
			sb.OrderBy(orderForward)
		}
	}

	filters := parser.AssignSearchFilters(sb, params.Query, config)

	return sb, filters, nil
}

func expandJSON(logAttributes map[string]string) map[string]interface{} {
	gqlLogAttributes := make(map[string]interface{}, len(logAttributes))
	for i, v := range logAttributes {
		gqlLogAttributes[i] = v
	}

	out, err := flat.Unflatten(gqlLogAttributes, nil)
	if err != nil {
		return gqlLogAttributes
	}

	return out
}

func KeysAggregated(ctx context.Context, client *Client, tableName string, projectID int, startDate time.Time, endDate time.Time, query *string, typeArg *modelInputs.KeyType, event *string) ([]*modelInputs.QueryKey, error) {
	chCtx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"max_rows_to_read": KeysMaxRows,
	}))

	sb := sqlbuilder.NewSelectBuilder()
	sb.Select("Key, Type, sum(Count)").
		From(tableName).
		Where(sb.Equal("ProjectId", projectID)).
		Where(fmt.Sprintf("Day >= toStartOfDay(%s)", sb.Var(startDate))).
		Where(fmt.Sprintf("Day <= toStartOfDay(%s)", sb.Var(endDate)))

	if query != nil && *query != "" {
		sb.Where(fmt.Sprintf("Key ILIKE %s", sb.Var("%"+*query+"%")))
	}

	if typeArg != nil && *typeArg == modelInputs.KeyTypeNumeric {
		sb.Where(sb.Equal("Type", typeArg))
	}

	if event != nil && *event != "" {
		sb.Where(
			sb.Or(
				fmt.Sprintf("Event = %s", sb.Var(*event)),
				"Event = ''",
			))
	}

	sb.GroupBy("1, 2").
		OrderBy("3 DESC, 1").
		Limit(25)

	sql, args := sb.BuildWithFlavor(sqlbuilder.ClickHouse)

	span, _ := util.StartSpanFromContext(chCtx, "readKeys", util.ResourceName(tableName))
	span.SetAttribute(string(semconv.DBNamespaceKey), tableName)
	span.SetAttribute(string(semconv.DBQueryTextKey), sql)
	span.SetAttribute(string(semconv.DBSystemKey), "clickhouse")
	span.SetAttribute(string(semconv.DBOperationNameKey), "SELECT")
	span.SetAttribute(string("db.query.summary"), "SELECT Key, Type, sum(Count) FROM "+tableName)
	span.SetAttribute("db.operation.parameters", args)

	rows, err := client.conn.Query(chCtx, sql, args...)

	if err != nil {
		return nil, err
	}

	keys := []*modelInputs.QueryKey{}
	for rows.Next() {
		var (
			key     string
			keyType string
			count   uint64
		)

		if err := rows.Scan(&key, &keyType, &count); err != nil {
			return nil, err
		}

		keys = append(keys, &modelInputs.QueryKey{
			Name: key,
			Type: modelInputs.KeyType(keyType),
		})
	}

	rows.Close()

	span.Finish(rows.Err())
	return keys, rows.Err()
}

func KeyValuesAggregated(ctx context.Context, client *Client, tableName string, projectID int, keyName string, startDate time.Time, endDate time.Time, query *string, limit *int, event *string) ([]string, error) {
	chCtx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"max_rows_to_read": KeyValuesMaxRows,
	}))

	limitCount := 500
	if limit != nil {
		limitCount = *limit
	}

	searchQuery := ""
	if query != nil {
		searchQuery = *query
	}

	sb := sqlbuilder.NewSelectBuilder()
	sb.Select("Value, sum(Count)").
		From(tableName).
		Where(sb.Equal("ProjectId", projectID)).
		Where(sb.Equal("Key", keyName)).
		Where(fmt.Sprintf("Value ILIKE %s", sb.Var("%"+searchQuery+"%"))).
		Where(fmt.Sprintf("Day >= toStartOfDay(%s)", sb.Var(startDate))).
		Where(fmt.Sprintf("Day <= toStartOfDay(%s)", sb.Var(endDate)))

	if event != nil && *event != "" {
		sb.Where(
			sb.Or(
				fmt.Sprintf("Event = %s", sb.Var(*event)),
				"Event = ''",
			))
	}

	sb.GroupBy("1").
		OrderBy("2 DESC, 1").
		Limit(limitCount)

	sql, args := sb.BuildWithFlavor(sqlbuilder.ClickHouse)

	span, _ := util.StartSpanFromContext(chCtx, "readKeyValues", util.ResourceName(tableName))
	span.SetAttribute(string(semconv.DBNamespaceKey), tableName)
	span.SetAttribute(string(semconv.DBQueryTextKey), sql)
	span.SetAttribute(string(semconv.DBSystemKey), "clickhouse")
	span.SetAttribute(string(semconv.DBOperationNameKey), "SELECT")
	span.SetAttribute(string("db.query.summary"), "SELECT Value, sum(Count) FROM "+tableName)
	span.SetAttribute("db.operation.parameters", args)

	rows, err := client.conn.Query(chCtx, sql, args...)
	if err != nil {
		return nil, err
	}

	values := []string{}
	for rows.Next() {
		var (
			value string
			count uint64
		)
		if err := rows.Scan(&value, &count); err != nil {
			return nil, err
		}

		values = append(values, value)
	}

	rows.Close()

	span.Finish(rows.Err())
	return values, rows.Err()
}

func KeyValueSuggestionsAggregated(ctx context.Context, client *Client, keyTableName string, valueTableName string, projectID int, startDate time.Time, endDate time.Time, keys []string) ([]*modelInputs.KeyValueSuggestion, error) {
	chCtx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"max_rows_to_read": KeyValuesMaxRows,
	}))

	// get all keys and values with rank
	rankSb := sqlbuilder.NewSelectBuilder()
	rankSb.Select("Key, Value, sum(Count) OVER (PARTITION By Key) as KeyCount, sum(Count) as ValueCount, row_number() OVER (PARTITION BY Key ORDER BY sum(Count) DESC) AS Rank").
		From(valueTableName).
		Where(rankSb.Equal("ProjectId", projectID)).
		Where(fmt.Sprintf("Day >= toStartOfDay(%s)", rankSb.Var(startDate))).
		Where(fmt.Sprintf("Day <= toStartOfDay(%s)", rankSb.Var(endDate))).
		Where(rankSb.In("Key", keys)).
		GroupBy("Key, Value, Count")

	// take top 5 values for each key
	sb := sqlbuilder.NewSelectBuilder()
	sb.Select("*").
		From(sb.BuilderAs(rankSb, "ranked_keys")).
		Where("Rank <= 5").
		OrderBy("Key, Rank")

	sql, args := sb.BuildWithFlavor(sqlbuilder.ClickHouse)

	span, _ := util.StartSpanFromContext(chCtx, "readKeyValueSuggestions", util.ResourceName(valueTableName))
	span.SetAttribute("Query", sql)
	span.SetAttribute("Table", valueTableName)
	span.SetAttribute("db.system", "clickhouse")

	rows, err := client.conn.Query(chCtx, sql, args...)
	if err != nil {
		return nil, err
	}

	keyValueMap := map[string][]*modelInputs.ValueSuggestion{}

	for rows.Next() {
		var (
			key        string
			value      string
			keyCount   uint64
			valueCount uint64
			rank       uint64
		)
		if err := rows.Scan(&key, &value, &keyCount, &valueCount, &rank); err != nil {
			return nil, err
		}

		// collect values per key
		keyValueMap[key] = append(keyValueMap[key], &modelInputs.ValueSuggestion{
			Value: value,
			Count: valueCount,
			Rank:  rank,
		})
	}

	// add values back to ordered keys
	keyValues := []*modelInputs.KeyValueSuggestion{}

	for _, keyValue := range keys {
		keyValues = append(keyValues, &modelInputs.KeyValueSuggestion{
			Key:    keyValue,
			Values: keyValueMap[keyValue],
		})
	}

	rows.Close()

	span.Finish(rows.Err())
	return keyValues, rows.Err()
}

func (client *Client) AllKeys(ctx context.Context, projectID int, startDate time.Time, endDate time.Time, query *string, typeArg *modelInputs.KeyType) ([]*modelInputs.QueryKey, error) {
	chCtx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"max_rows_to_read": KeysMaxRows,
	}))

	limitCount := 25

	allTables := []string{EventKeysTable, LogKeysTable, TraceKeysTable, SessionKeysTable}

	builders := []sqlbuilder.Builder{}
	for _, table := range allTables {
		sb := sqlbuilder.NewSelectBuilder()
		sb.Select("Key, sum(Count) / max(sum(Count)) over () as PctCount").
			From(table).
			Where(sb.Equal("ProjectId", projectID)).
			Where(fmt.Sprintf("Day >= toStartOfDay(%s)", sb.Var(startDate))).
			Where(fmt.Sprintf("Day <= toStartOfDay(%s)", sb.Var(endDate)))

		if query != nil && *query != "" {
			sb.Where(fmt.Sprintf("Key ILIKE %s", sb.Var("%"+*query+"%")))
		}

		if typeArg != nil && *typeArg == modelInputs.KeyTypeNumeric {
			sb.Where(sb.Equal("Type", typeArg))
		}

		sb.GroupBy("1").
			OrderBy("2 DESC, 1").
			Limit(limitCount)

		builders = append(builders, sb)
	}

	sb := sqlbuilder.NewSelectBuilder()
	sql, args := sb.Select("Key, sum(PctCount)").
		From(sb.BuilderAs(sqlbuilder.UnionAll(builders...), "inner")).
		GroupBy("1").
		BuildWithFlavor(sqlbuilder.ClickHouse)

	span, _ := util.StartSpanFromContext(chCtx, "readAllKeys")
	span.SetAttribute(string(semconv.DBNamespaceKey), EventKeysTable)
	span.SetAttribute(string(semconv.DBQueryTextKey), sql)
	span.SetAttribute(string(semconv.DBSystemKey), "clickhouse")
	span.SetAttribute(string(semconv.DBOperationNameKey), "SELECT")
	span.SetAttribute(string("db.query.summary"), "SELECT Key, sum(PctCount) FROM "+EventKeysTable)
	span.SetAttribute("db.operation.parameters", args)

	rows, err := client.conn.Query(chCtx, sql, args...)

	if err != nil {
		return nil, err
	}

	type queryKeyWithCount struct {
		*modelInputs.QueryKey
		count float64
	}

	keys := []queryKeyWithCount{}
	for rows.Next() {
		var (
			key   string
			count float64
		)

		if err := rows.Scan(&key, &count); err != nil {
			return nil, err
		}

		keys = append(keys, queryKeyWithCount{
			QueryKey: &modelInputs.QueryKey{Name: key},
			count:    count})
	}
	rows.Close()
	span.Finish(rows.Err())
	if rows.Err() != nil {
		return nil, rows.Err()
	}

	// Add all matching default keys into the results
	defaultKeys := lo.Map(modelInputs.AllReservedErrorsJoinedKey, func(k modelInputs.ReservedErrorsJoinedKey, _ int) *modelInputs.QueryKey {
		return &modelInputs.QueryKey{Name: string(k), Type: modelInputs.KeyTypeString}
	})

	defaultKeys = append(defaultKeys, defaultLogKeys...)
	defaultKeys = append(defaultKeys, defaultEventKeys...)
	defaultKeys = append(defaultKeys, defaultTraceKeys...)
	defaultKeys = append(defaultKeys, defaultSessionsKeys...)

	defaultKeys = lo.Filter(defaultKeys, func(k *modelInputs.QueryKey, _ int) bool {
		return query == nil || strings.Contains(strings.ToLower(string(k.Name)), strings.ToLower(*query))
	})

	keys = append(keys, lo.Map(defaultKeys, func(k *modelInputs.QueryKey, _ int) queryKeyWithCount {
		return queryKeyWithCount{
			k,
			1.0}
	})...)

	grouped := lo.GroupBy(keys, func(k queryKeyWithCount) string {
		return k.Name
	})

	keysAggregated := []queryKeyWithCount{}
	for k, v := range grouped {
		totalCount := lo.SumBy(v, func(k queryKeyWithCount) float64 {
			return k.count
		})

		keysAggregated = append(keysAggregated, queryKeyWithCount{
			QueryKey: &modelInputs.QueryKey{Name: k},
			count:    totalCount,
		})
	}

	sort.Slice(keysAggregated, func(i, j int) bool {
		return keysAggregated[i].count > keysAggregated[j].count
	})

	if len(keysAggregated) > limitCount {
		keysAggregated = keysAggregated[:limitCount]
	}

	return lo.Map(keysAggregated, func(k queryKeyWithCount, _ int) *modelInputs.QueryKey {
		return k.QueryKey
	}), nil
}

func (client *Client) AllKeyValues(ctx context.Context, projectID int, keyName string, startDate time.Time, endDate time.Time, query *string, limit *int) ([]string, error) {
	chCtx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"max_rows_to_read": AllKeyValuesMaxRows,
	}))

	limitCount := 500
	if limit != nil {
		limitCount = *limit
	}

	searchQuery := ""
	if query != nil {
		searchQuery = *query
	}

	allTables := []string{EventKeyValuesTable, LogKeyValuesTable, TraceKeyValuesTable}

	builders := []sqlbuilder.Builder{}
	for _, table := range allTables {
		sb := sqlbuilder.NewSelectBuilder()
		sb.Select("Value, sum(Count) / max(sum(Count)) over () as PctCount").
			From(table).
			Where(sb.Equal("ProjectId", projectID)).
			Where(sb.Equal("Key", keyName)).
			Where(fmt.Sprintf("Value ILIKE %s", sb.Var("%"+searchQuery+"%"))).
			Where(fmt.Sprintf("Day >= toStartOfDay(%s)", sb.Var(startDate))).
			Where(fmt.Sprintf("Day <= toStartOfDay(%s)", sb.Var(endDate))).
			GroupBy("1").
			OrderBy("2 DESC, 1").
			Limit(limitCount)
		builders = append(builders, sb)
	}

	// Sessions key values are stored in a different format
	sessionsSb := sqlbuilder.NewSelectBuilder()
	sessionsSb.Select("Value, count() / max(count()) over () as PctCount").
		From(FieldsTable).
		Where(sessionsSb.Equal("ProjectID", projectID)).
		Where(sessionsSb.Equal("Name", keyName)).
		Where(fmt.Sprintf("Value ILIKE %s", sessionsSb.Var("%"+searchQuery+"%"))).
		Where(fmt.Sprintf("SessionCreatedAt >= %s", sessionsSb.Var(startDate))).
		Where(fmt.Sprintf("SessionCreatedAt <= %s", sessionsSb.Var(endDate))).
		GroupBy("1").
		OrderBy("2 DESC, 1").
		Limit(limitCount)
	builders = append(builders, sessionsSb)

	sb := sqlbuilder.NewSelectBuilder()
	sql, args := sb.Select("Value, sum(PctCount)").
		From(sb.BuilderAs(sqlbuilder.UnionAll(builders...), "inner")).
		GroupBy("1").
		OrderBy("2 DESC").
		Limit(limitCount).
		BuildWithFlavor(sqlbuilder.ClickHouse)

	span, _ := util.StartSpanFromContext(chCtx, "readAllKeyValues")
	span.SetAttribute(string(semconv.DBNamespaceKey), FieldsTable)
	span.SetAttribute(string(semconv.DBQueryTextKey), sql)
	span.SetAttribute(string(semconv.DBSystemKey), "clickhouse")
	span.SetAttribute(string(semconv.DBOperationNameKey), "SELECT")
	span.SetAttribute(string("db.query.summary"), "SELECT Value, sum(PctCount) FROM "+FieldsTable)
	span.SetAttribute("db.operation.parameters", args)

	rows, err := client.conn.Query(chCtx, sql, args...)
	if err != nil {
		return nil, err
	}

	values := []string{}
	for rows.Next() {
		var (
			value string
			count float64
		)
		if err := rows.Scan(&value, &count); err != nil {
			return nil, err
		}

		values = append(values, value)
	}

	rows.Close()

	span.Finish(rows.Err())
	return values, rows.Err()
}

func getChildValue(value reflect.Value, key string) (string, bool) {
	for _, part := range strings.Split(key, ".") {
		if !value.IsValid() {
			return "", false
		}
		value = value.FieldByName(part)
		if value.Kind() == reflect.Pointer {
			value = value.Elem()
		}
	}
	if value.IsValid() {
		return repr(value), true
	}
	return "", false
}

// clickhouse token - https://clickhouse.com/docs/en/sql-reference/functions/splitting-merging-functions#tokens
var nonAlphaNumericChars = regexp.MustCompile(`[^\w:*]`)

// strip toString() wrapper around filter keys to match the KeysToColumns map
var keyWrapper = regexp.MustCompile(`toString\((\w+)\)`)

func matchFilter[TObj interface{}](row *TObj, config model.TableConfig, filter *listener.FilterOperation) (bool, error) {
	key := filter.Key
	groups := keyWrapper.FindStringSubmatch(key)
	if len(groups) > 0 {
		key = groups[1]
	}
	bodyFilter := config.BodyColumn != "" && filter.Column == "" && key == config.BodyColumn
	v := reflect.ValueOf(*row)

	rowBodyTerms := map[string]bool{}
	if bodyFilter {
		body := v.FieldByName(config.BodyColumn).String()
		for _, field := range nonAlphaNumericChars.Split(body, -1) {
			if field != "" {
				rowBodyTerms[field] = true
			}
		}
		for _, bf := range filter.Values {
			if filter.Operator == listener.OperatorRegExp {
				pat, err := regexp.Compile(bf)
				if err == nil {
					matches := pat.MatchString(body)
					shouldMatch := filter.Operator == listener.OperatorRegExp

					if (shouldMatch && !matches) || (!shouldMatch && matches) {
						return false, nil
					}
				}
			} else if strings.Contains(bf, "%") {
				pat, err := regexp.Compile(strings.ReplaceAll(regexp.QuoteMeta(bf), "%", ".*"))
				// this may over match if the expression cannot be compiled,
				// but we'd prefer to over match as this fn is used to determine sampling
				if err != nil {
					return false, err
				}
				if !pat.MatchString(body) {
					return false, nil
				}
			} else if !rowBodyTerms[bf] {
				return false, nil
			}
		}
		return true, nil
	}

	var rowValue string
	if chKey, ok := config.KeysToColumns[key]; ok {
		if val, ok := getChildValue(v, chKey); ok {
			rowValue = val
		} else {
			rowValue = repr(v.FieldByName(chKey))
		}
	} else if field := v.FieldByName(key); field.IsValid() {
		rowValue = repr(field)
	} else if val, ok := getChildValue(v, key); ok {
		rowValue = val
	} else if col := model.GetAttributesColumn(config.AttributesColumns, ""); col != "" {
		value := v.FieldByName(col)
		if value.Kind() == reflect.Map {
			rowValue = repr(value.MapIndex(reflect.ValueOf(key)))
		} else if value.Kind() == reflect.Slice {
			// assume that the key is a 'field' in `type_name` format
			fieldParts := strings.SplitN(key, "_", 2)
			for i := 0; i < value.Len(); i++ {
				fieldType := value.Index(i).Elem().FieldByName("Type").String()
				name := value.Index(i).Elem().FieldByName("Name").String()
				if fieldType == fieldParts[0] && name == fieldParts[1] {
					rowValue = repr(value.Index(i).Elem().FieldByName("Value"))
					break
				}
			}
		}
	} else {
		return true, e.New(fmt.Sprintf("invalid filter %s", key))
	}
	for _, v := range filter.Values {
		if filter.Operator == listener.OperatorRegExp {
			pat, err := regexp.Compile(v)
			if err == nil {
				matches := pat.MatchString(rowValue)
				shouldMatch := filter.Operator == listener.OperatorRegExp

				if (shouldMatch && !matches) || (!shouldMatch && matches) {
					return false, nil
				}
			}
		} else if strings.Contains(v, "%") {
			if matched, _ := regexp.Match(strings.ReplaceAll(v, "%", ".*"), []byte(rowValue)); !matched {
				return false, nil
			}
		} else if filter.Operator == listener.OperatorNotEqual {
			if rowValue == strings.Replace(v, "-", "", 1) {
				return false, nil
			}
		} else if v != rowValue {
			return false, nil
		}
	}
	return true, nil
}

func matchesQuery[TObj interface{}](row *TObj, config model.TableConfig, filters listener.Filters, op listener.Operator) bool {
	// if multiple filters are passed in, assume an AND operation between them
	for _, filter := range filters {
		switch filter.Operator {
		case listener.OperatorAnd:
			for _, childFilter := range filter.Filters {
				if !matchesQuery[TObj](row, config, listener.Filters{childFilter}, filter.Operator) {
					return false
				}
			}
		case listener.OperatorOr:
			var anyMatch bool
			for _, childFilter := range filter.Filters {
				if matchesQuery[TObj](row, config, listener.Filters{childFilter}, filter.Operator) {
					anyMatch = true
					break
				}
			}
			if !anyMatch {
				return false
			}
		case listener.OperatorNot:
			return !matchesQuery[TObj](row, config, listener.Filters{filter.Filters[0]}, filter.Operator)
		default:
			matches, err := matchFilter(row, config, filter)
			if err != nil {
				if op == listener.OperatorOr {
					return false
				} else if op == listener.OperatorNot {
					return true
				} else {
					return true
				}
			}
			if !matches {
				return false
			}
		}
	}
	return true
}

func getLimitFnStr(aggregator modelInputs.MetricAggregator, column string) string {
	switch aggregator {
	case modelInputs.MetricAggregatorCount:
		return "count()"
	case modelInputs.MetricAggregatorCountDistinctKey, modelInputs.MetricAggregatorCountDistinct:
		return fmt.Sprintf("count(distinct %s)", column)
	case modelInputs.MetricAggregatorMin:
		return fmt.Sprintf("min(%s)", column)
	case modelInputs.MetricAggregatorAvg:
		return fmt.Sprintf("avg(%s)", column)
	case modelInputs.MetricAggregatorP50:
		return fmt.Sprintf("quantile(.5)(%s)", column)
	case modelInputs.MetricAggregatorP90:
		return fmt.Sprintf("quantile(.9)(%s)", column)
	case modelInputs.MetricAggregatorP95:
		return fmt.Sprintf("quantile(.95)(%s)", column)
	case modelInputs.MetricAggregatorP99:
		return fmt.Sprintf("quantile(.99)(%s)", column)
	case modelInputs.MetricAggregatorMax:
		return fmt.Sprintf("max(%s)", column)
	case modelInputs.MetricAggregatorSum:
		return fmt.Sprintf("sum(%s)", column)
	}
	return ""
}

func getFnStr(aggregator modelInputs.MetricAggregator, column string, useSampling bool, useState bool) string {
	switch aggregator {
	case modelInputs.MetricAggregatorCount:
		if useState {
			return "countState()"
		} else if useSampling {
			return "round(count() * any(_sample_factor))"
		} else {
			return "round(count() * 1.0)"
		}
	case modelInputs.MetricAggregatorCountDistinctKey, modelInputs.MetricAggregatorCountDistinct:
		if useState {
			return fmt.Sprintf("uniqState(toString(%s))", column)
		}
		return fmt.Sprintf("round(count(distinct %s) * 1.0)", column)
	case modelInputs.MetricAggregatorMin:
		if useState {
			return fmt.Sprintf("minState(toFloat64(%s))", column)
		}
		return fmt.Sprintf("toFloat64(min(%s))", column)
	case modelInputs.MetricAggregatorAvg:
		if useState {
			return fmt.Sprintf("avgState(toFloat64(%s))", column)
		}
		return fmt.Sprintf("avg(%s)", column)
	case modelInputs.MetricAggregatorP50:
		if useState {
			return fmt.Sprintf("quantileState(.5)(toFloat64(%s))", column)
		}
		return fmt.Sprintf("quantile(.5)(%s)", column)
	case modelInputs.MetricAggregatorP90:
		if useState {
			return fmt.Sprintf("quantileState(.9)(toFloat64(%s))", column)
		}
		return fmt.Sprintf("quantile(.9)(%s)", column)
	case modelInputs.MetricAggregatorP95:
		if useState {
			return fmt.Sprintf("quantileState(.95)(toFloat64(%s))", column)
		}
		return fmt.Sprintf("quantile(.95)(%s)", column)
	case modelInputs.MetricAggregatorP99:
		if useState {
			return fmt.Sprintf("quantileState(.99)(toFloat64(%s))", column)
		}
		return fmt.Sprintf("quantile(.99)(%s)", column)
	case modelInputs.MetricAggregatorMax:
		if useState {
			return fmt.Sprintf("maxState(toFloat64(%s))", column)
		}
		return fmt.Sprintf("toFloat64(max(%s))", column)
	case modelInputs.MetricAggregatorSum:
		if useState {
			return fmt.Sprintf("sumState(toFloat64(%s))", column)
		} else if useSampling {
			return fmt.Sprintf("sum(%s) * any(_sample_factor)", column)
		} else {
			return fmt.Sprintf("sum(%s) * 1.0", column)
		}
	}
	return ""
}

func getAttributeFilterCol(fromSb *sqlbuilder.SelectBuilder, sampleableConfig SampleableTableConfig, valueUnescaped, op string) (column string) {
	attributesColumn := model.GetAttributesColumn(sampleableConfig.tableConfig.AttributesColumns, valueUnescaped)
	value := fromSb.Var(valueUnescaped)
	column = fmt.Sprintf("%s[%s]", attributesColumn, value)
	if sampleableConfig.tableConfig.AttributesTable != "" {
		transform := "v"
		if op != "" {
			transform = fmt.Sprintf("%s(%s)", op, transform)
		}
		// use the first value from the resulting array, if more than one value provided
		column = fmt.Sprintf("(arrayMap((k, v) -> %s, arrayFilter((k, v) -> k = %s, %s)))[1]", transform, value, attributesColumn)
	} else {
		if op != "" {
			column = fmt.Sprintf("%s(%s)", op, column)
		}
	}
	return
}

func applyBlockFilter(sb *sqlbuilder.SelectBuilder, input ReadMetricsInput) {
	if input.SavedMetricState != nil && len(input.SavedMetricState.BlockNumberInfo) > 0 {
		partSb := sqlbuilder.NewSelectBuilder()
		partSb.Select("name").
			From("system.parts").
			Where(partSb.Equal("table", input.SampleableConfig.tableConfig.TableName)).
			Where("active")

		var orExprs []string
		for _, info := range input.SavedMetricState.BlockNumberInfo {
			orExprs = append(orExprs, partSb.And(
				partSb.Equal("partition", info.Partition),
				partSb.GreaterThan("max_block_number", info.LastBlockNumber),
			))
		}
		partSb.Where(partSb.Or(orExprs...))

		sb.Where(sb.In("_part", partSb))

		orExprs = []string{}
		for _, info := range input.SavedMetricState.BlockNumberInfo {
			orExprs = append(orExprs, partSb.And(
				sb.Equal("_partition_id", strings.ReplaceAll(info.Partition, "-", "")),
				sb.GreaterThan("_block_number", info.LastBlockNumber),
			))
		}
		sb.Where(sb.Or(orExprs...))
	}
}

func (client *Client) saveMetricHistory(ctx context.Context, sb *sqlbuilder.SelectBuilder, input ReadMetricsInput) error {
	if input.SavedMetricState == nil {
		return nil
	}

	bucketCount := 1
	if input.BucketCount != nil {
		bucketCount = *input.BucketCount
	}

	insertCols := []string{"MetricId", "Timestamp", "MaxBlockNumberState"}
	selectCols := []string{fmt.Sprintf(`'%s'`, input.SavedMetricState.MetricId),
		fmt.Sprintf("fromUnixTimestamp(toInt64(%s*(%s-%s)/%d + %s))", bucketIndexAlias, maxAlias, minAlias, bucketCount, minAlias),
		"max_block_number",
		"metric_value0",
	}

	aggregator := modelInputs.MetricAggregatorCount
	if len(input.Expressions) > 0 {
		aggregator = input.Expressions[0].Aggregator
	}

	switch aggregator {
	case modelInputs.MetricAggregatorCount:
		insertCols = append(insertCols, "CountState")
	case modelInputs.MetricAggregatorCountDistinct, modelInputs.MetricAggregatorCountDistinctKey:
		insertCols = append(insertCols, "UniqState")
	case modelInputs.MetricAggregatorMin:
		insertCols = append(insertCols, "MinState")
	case modelInputs.MetricAggregatorAvg:
		insertCols = append(insertCols, "AvgState")
	case modelInputs.MetricAggregatorMax:
		insertCols = append(insertCols, "MaxState")
	case modelInputs.MetricAggregatorSum:
		insertCols = append(insertCols, "SumState")
	case modelInputs.MetricAggregatorP50:
		insertCols = append(insertCols, "P50State")
	case modelInputs.MetricAggregatorP90:
		insertCols = append(insertCols, "P90State")
	case modelInputs.MetricAggregatorP95:
		insertCols = append(insertCols, "P95State")
	case modelInputs.MetricAggregatorP99:
		insertCols = append(insertCols, "P99State")
	}

	if len(input.GroupBy) > 0 {
		insertCols = append(insertCols, "GroupByKey")
		selectCols = append(selectCols, "g0")
	}

	insertBuilder := sqlbuilder.NewInsertBuilder()
	insertSb := insertBuilder.InsertInto(MetricHistoryTable).Cols(insertCols...).Select(selectCols...)
	insertSb.From(insertSb.BuilderAs(sb, "innerSelect"))

	sql, args := insertBuilder.BuildWithFlavor(sqlbuilder.ClickHouse)

	err := client.conn.Exec(
		ctx,
		sql,
		args...,
	)

	return err
}

type SamplingStats struct {
	Database string `ch:"database"`
	Table    string `ch:"table"`
	Parts    uint64 `ch:"parts"`
	Rows     uint64 `ch:"rows"`
	Marks    uint64 `ch:"marks"`
}

func (client *Client) getSamplingStats(ctx context.Context, tables []string, projectIds []int, dateRange modelInputs.DateRangeRequiredInput) (map[string]SamplingStats, error) {
	tables = lo.Uniq(tables)
	projectIds = lo.Uniq(projectIds)

	builders := []sqlbuilder.Builder{}
	for _, table := range tables {
		sb := sqlbuilder.NewSelectBuilder()
		sb.Select("1")
		sb.From(table)
		sb.Where(sb.In("ProjectId", projectIds))
		sb.Where(sb.GreaterEqualThan("Timestamp", dateRange.StartDate))
		sb.Where(sb.LessEqualThan("Timestamp", dateRange.EndDate))
		builders = append(builders, sb)
	}

	sb := sqlbuilder.UnionAll(builders...)
	sql, args := sb.BuildWithFlavor(sqlbuilder.ClickHouse)
	sql = "EXPLAIN ESTIMATE " + sql

	span, ctx := util.StartSpanFromContext(ctx, "getSamplingStats.query")
	span.SetAttribute("sql", sql)
	span.SetAttribute("args", args)
	rows, err := client.conn.Query(
		ctx,
		sql,
		args...,
	)
	span.Finish(err)
	if err != nil {
		return nil, err
	}

	results := []SamplingStats{}
	for rows.Next() {
		var result SamplingStats
		if err := rows.ScanStruct(&result); err != nil {
			return nil, err
		}
		results = append(results, result)
	}

	statsByTable := lo.KeyBy(results, func(s SamplingStats) string {
		return s.Table
	})

	return statsByTable, nil
}

const groupSuffix = "_group"
const seriesSuffix = "_series"

func (client *Client) readMetricsSql(ctx context.Context, input ReadMetricsInput, config model.TableConfig) (*modelInputs.MetricsBuckets, error) {
	sql, err := transformSql(config, input)
	if err != nil {
		return nil, err
	}

	if strings.Contains(sql, projectIdSetting) {
		return nil, e.New("User cannot modify project id setting in query")
	}

	span, ctx := util.StartSpanFromContext(ctx, "readMetric.sql.query")
	span.SetAttribute("sql", sql)

	chCtx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		projectIdSetting: clickhouse.CustomSetting{Value: strconv.Itoa(input.ProjectIDs[0])},
	}))

	rows, err := client.connReadonly.Query(
		chCtx,
		sql,
	)

	span.Finish(err)

	if err != nil {
		return nil, err
	}

	metrics := &modelInputs.MetricsBuckets{
		Buckets: []*modelInputs.MetricBucket{},
	}

	types := rows.ColumnTypes()
	scanResults := []interface{}{}
	selectNames := []string{}
	for _, t := range types {
		typ := t.ScanType()
		rv := reflect.New(typ).Interface()
		scanResults = append(scanResults, rv)

		selectNames = append(selectNames, t.Name())
	}

	results := map[int]*float64{}
	for i := 0; rows.Next(); i++ {
		if err := rows.Scan(scanResults...); err != nil {
			return nil, err
		}

		var bucketId uint64
		var bucketValue *float64
		groups := []string{}
		for idx, r := range scanResults {
			rv := reflect.ValueOf(r)

			// Unwrap nested pointers - e.g. `Nullable(Nullable(Int))` -> `Int`
			for ; rv.Kind() == reflect.Pointer && !rv.IsNil(); rv = rv.Elem() {
			}

			columnName := selectNames[idx]

			isSeries := strings.HasSuffix(columnName, seriesSuffix)
			isGroup := strings.HasSuffix(columnName, groupSuffix)

			switch rv.Kind() {
			case reflect.Struct:
				switch typed := rv.Interface().(type) {
				case time.Time:
					if isSeries {
						results[idx] = pointy.Float64(float64(typed.Unix()))
					} else if isGroup {
						groups = append(groups, typed.Format(time.RFC3339))
					} else {
						bucketId = uint64(typed.Unix())
						bucketValue = pointy.Float64(float64(typed.Unix()))
					}
				}
			case reflect.String:
				if isSeries {
					// Attempt to parse as a float, ignoring any conversion errors
					value, err := strconv.ParseFloat(rv.String(), 64)
					if err == nil {
						results[idx] = pointy.Float64(value)
					}
				} else {
					groups = append(groups, rv.String())
				}
			case reflect.Bool:
				if isSeries {
					if rv.Bool() {
						results[idx] = pointy.Float64(1.0)
					} else {
						results[idx] = pointy.Float64(0.0)
					}
				} else {
					groups = append(groups, strconv.FormatBool(rv.Bool()))
				}
			case reflect.Float32, reflect.Float64:
				if isGroup {
					groups = append(groups, strconv.FormatFloat(rv.Float(), 'f', -1, 64))
				} else {
					results[idx] = pointy.Float64(rv.Float())
				}
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				if isGroup {
					groups = append(groups, strconv.FormatInt(rv.Int(), 10))
				} else {
					results[idx] = pointy.Float64(float64(rv.Int()))
				}
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				if isGroup {
					groups = append(groups, strconv.FormatUint(rv.Uint(), 10))
				} else {
					results[idx] = pointy.Float64(float64(rv.Uint()))
				}
			case reflect.Pointer:
				results[idx] = nil
			}
		}

		for idx, r := range results {
			columnName := selectNames[idx]
			columnName = strings.TrimSuffix(columnName, seriesSuffix)
			columnName = strings.TrimSuffix(columnName, groupSuffix)
			metrics.Buckets = append(metrics.Buckets, &modelInputs.MetricBucket{
				BucketID:    bucketId,
				BucketValue: bucketValue,
				Group:       groups,
				MetricType:  modelInputs.MetricAggregator(columnName),
				MetricValue: r,
			})
		}
	}

	return metrics, err
}

type BucketingInfo struct {
	NewSelectItems     []string
	NewAttributeFields []string
	BucketCount        int
}

func getBucketing(config model.TableConfig, input ReadMetricsInput, fromSb *sqlbuilder.SelectBuilder) BucketingInfo {
	nBuckets := 48
	if input.BucketWindow == nil {
		if input.BucketBy == modelInputs.MetricBucketByNone.String() {
			nBuckets = 1
		} else if input.BucketCount != nil {
			nBuckets = *input.BucketCount
		}
		if nBuckets > MaxBuckets && !input.NoBucketMax {
			nBuckets = MaxBuckets
		}
		if nBuckets < 1 {
			nBuckets = 1
		}
	} else {
		nBuckets = int(int64(input.Params.DateRange.EndDate.Sub(input.Params.DateRange.StartDate).Seconds()) / int64(*input.BucketWindow))
		if nBuckets > MaxBuckets && !input.NoBucketMax {
			nBuckets = MaxBuckets
			input.Params.DateRange.StartDate = input.Params.DateRange.EndDate.Add(-1 * time.Duration(MaxBuckets**input.BucketWindow) * time.Second)
		}
	}

	startTimestamp := input.Params.DateRange.StartDate.Unix()
	endTimestamp := input.Params.DateRange.EndDate.Unix()

	bucketExpr := "toFloat64(Timestamp)"
	attributeFields := []string{}
	if input.BucketBy != modelInputs.MetricBucketByNone.String() && input.BucketBy != modelInputs.MetricBucketByTimestamp.String() {
		if col, found := config.KeysToColumns[strings.ToLower(input.BucketBy)]; found {
			bucketExpr = fmt.Sprintf("toFloat64(%s)", col)
		} else {
			bucketExpr = getAttributeFilterCol(fromSb, input.SampleableConfig, input.BucketBy, "toFloat64OrNull")
			attributeFields = append(attributeFields, input.BucketBy)
		}
	}

	minExpr := fmt.Sprintf("MIN(%s) OVER ()", bucketExpr)
	maxExpr := fmt.Sprintf("MAX(%s) OVER ()", bucketExpr)

	if input.BucketBy == modelInputs.MetricBucketByTimestamp.String() || input.BucketBy == modelInputs.MetricBucketByNone.String() {
		minExpr = fmt.Sprintf("%d.0", startTimestamp)
		maxExpr = fmt.Sprintf("%d.0", endTimestamp)
	}

	bucketIdxExpr := fmt.Sprintf("toUInt64(intDiv((%s - %s) * %d, (%s - %s)))", bucketExpr, minExpr, nBuckets, maxExpr, minExpr)
	newSelectCols := []string{fromSb.As(bucketIdxExpr, bucketIndexAlias), fromSb.As(minExpr, minAlias), fromSb.As(maxExpr, maxAlias)}

	return BucketingInfo{
		NewSelectItems:     newSelectCols,
		NewAttributeFields: attributeFields,
		BucketCount:        nBuckets,
	}
}

func (client *Client) ReadMetrics(ctx context.Context, input ReadMetricsInput) (*modelInputs.MetricsBuckets, error) {
	if input.Params.DateRange == nil {
		input.Params.DateRange = &modelInputs.DateRangeRequiredInput{
			StartDate: time.Now().Add(-time.Hour * 24 * 30),
			EndDate:   time.Now(),
		}
	}

	sampleRatio := 0.0
	if input.SampleableConfig.sampleSizeRows != 0 {
		originalTable := input.SampleableConfig.tableConfig.TableName
		samplingTable := input.SampleableConfig.samplingTableConfig.TableName

		samplingStats, err := client.getSamplingStats(ctx,
			[]string{originalTable, samplingTable},
			input.ProjectIDs,
			*input.Params.DateRange)
		if err != nil {
			return nil, err
		}

		if samplingStats[originalTable].Rows > input.SampleableConfig.sampleSizeRows {
			sampleRatio = float64(input.SampleableConfig.sampleSizeRows) / float64(samplingStats[samplingTable].Rows)
		}
	}

	span, ctx := util.StartSpanFromContext(ctx, "clickhouse.readMetrics")
	span.SetAttribute("project_ids", input.ProjectIDs)
	span.SetAttribute(string(semconv.DBNamespaceKey), input.SampleableConfig.tableConfig.TableName)
	span.SetAttribute(string(semconv.DBQueryTextKey), input.Sql)
	span.SetAttribute(string(semconv.DBSystemKey), "clickhouse")
	span.SetAttribute(string(semconv.DBOperationNameKey), "SELECT")
	span.SetAttribute(string("db.operation.parameters"), input.Params)
	defer span.Finish()

	if len(input.Expressions) == 0 {
		return nil, errors.New("no expressions provided")
	}
	if input.Params.DateRange == nil {
		input.Params.DateRange = &modelInputs.DateRangeRequiredInput{
			StartDate: time.Now().Add(-time.Hour * 24 * 30),
			EndDate:   time.Now(),
		}
	}

	var config model.TableConfig
	useSampling := sampleRatio != 0
	if useSampling {
		config = input.SampleableConfig.samplingTableConfig
		config.TableName = fmt.Sprintf("%s SAMPLE %f", config.TableName, sampleRatio)
	} else {
		config = input.SampleableConfig.tableConfig
	}
	for _, e := range input.Expressions {
		if e.Aggregator == modelInputs.MetricAggregatorCountDistinctKey {
			config.DefaultFilter = ""
		}
	}

	if input.Sql != nil {
		return client.readMetricsSql(ctx, input, config)
	}

	fromSb, filters, err := makeSelectBuilder(
		config,
		nil,
		input.ProjectIDs,
		input.Params,
		Pagination{CountOnly: true},
	)
	if err != nil {
		return nil, err
	}

	applyBlockFilter(fromSb, input)

	attributeFields := getAttributeFields(config, filters)

	bucketingInfo := getBucketing(config, input, fromSb)
	selectCols := bucketingInfo.NewSelectItems
	attributeFields = append(attributeFields, bucketingInfo.NewAttributeFields...)

	if input.SavedMetricState != nil {
		selectCols = append(selectCols, fromSb.As("maxState(_block_number) OVER ()", "max_block_number"))
	}

	if useSampling {
		selectCols = append(selectCols, "_sample_factor")
	} else {
		selectCols = append(selectCols, fromSb.As("1.0", "_sample_factor"))
	}

	for idx, e := range input.Expressions {
		var col string
		if col = config.KeysToColumns[strings.ToLower(e.Column)]; col == "" {
			col = getAttributeFilterCol(fromSb, input.SampleableConfig, e.Column, "")
			attributeFields = append(attributeFields, e.Column)
		}

		var metricExpr = col
		if e.Aggregator != modelInputs.MetricAggregatorCountDistinct && e.Aggregator != modelInputs.MetricAggregatorCountDistinctKey {
			if reservedCol, found := config.KeysToColumns[strings.ToLower(e.Column)]; found {
				metricExpr = fmt.Sprintf("toFloat64(%s)", reservedCol)
			} else if input.SampleableConfig.tableConfig.MetricColumn != nil {
				metricExpr = *input.SampleableConfig.tableConfig.MetricColumn
			} else {
				metricExpr = fmt.Sprintf("toFloat64OrNull(%s)", col)
			}
		}

		if e.Aggregator == modelInputs.MetricAggregatorCountDistinctKey {
			metricExpr = getAttributeFilterCol(fromSb, input.SampleableConfig, highlight.TraceKeyAttribute, "")
			attributeFields = append(attributeFields, highlight.TraceKeyAttribute)
		}

		if e.Aggregator == modelInputs.MetricAggregatorCount || e.Column == "" {
			metricExpr = "1.0"
		}

		selectCols = append(selectCols, fromSb.As(metricExpr, fmt.Sprintf("metric_input%d", idx)))
	}

	groupAliases := []string{}
	for idx, group := range input.GroupBy {
		groupCol := ""
		if col, found := config.KeysToColumns[group]; found {
			groupCol = fmt.Sprintf("toString(%s)", col)
		} else {
			groupCol = getAttributeFilterCol(fromSb, input.SampleableConfig, group, "toString")
			attributeFields = append(attributeFields, group)
		}

		groupAlias := fmt.Sprintf("g%d", idx)
		groupAliases = append(groupAliases, groupAlias)

		selectCols = append(selectCols, fromSb.As(groupCol, groupAlias))

		fromSb.Where(fromSb.NotEqual(groupAlias, ""))
	}

	limitCount := 10
	if input.Limit != nil {
		limitCount = *input.Limit
	}
	if limitCount < 1 {
		limitCount = 1
	}
	useLimit := input.LimitAggregator != nil && len(input.GroupBy) > 0 && limitCount != NoLimit
	if useLimit {
		col := ""
		if input.LimitColumn != nil {
			col = *input.LimitColumn
		}
		if topCol, found := config.KeysToColumns[col]; found {
			col = topCol
		} else {
			attributeFields = append(attributeFields, col)
			col = getAttributeFilterCol(fromSb, input.SampleableConfig, col, "toFloat64OrNull")
		}

		limitExpr := fmt.Sprintf("%s OVER (PARTITION BY %s) as limit_metric",
			getLimitFnStr(*input.LimitAggregator, col),
			strings.Join(groupAliases, ", "))

		selectCols = append(selectCols, limitExpr)
	}

	fromSb.Select(selectCols...)

	groupByCols := []string{bucketIndexAlias}
	orderByCols := []string{bucketIndexAlias}
	if useLimit {
		groupByCols = append(groupByCols, "limit_metric")
		orderByCols = append(orderByCols, "limit_rank")
	}
	groupByCols = append(groupByCols, groupAliases...)
	orderByCols = append(orderByCols, groupAliases...)

	addAttributes(config, attributeFields, input.ProjectIDs, input.Params, fromSb)

	innerSb := fromSb
	fromSb = sqlbuilder.NewSelectBuilder()
	outerSelect := []string{bucketIndexAlias, "any(_sample_factor) as sample_factor", fmt.Sprintf("any(%s) as %s", minAlias, minAlias), fmt.Sprintf("any(%s) as %s", maxAlias, maxAlias)}
	if input.SavedMetricState != nil {
		outerSelect = append(outerSelect, "any(max_block_number) as max_block_number")
	}
	for idx, e := range input.Expressions {
		outerSelect = append(outerSelect, fmt.Sprintf("%s as metric_value%d", getFnStr(e.Aggregator, fmt.Sprintf("metric_input%d", idx), true, input.SavedMetricState != nil), idx))
	}
	outerSelect = append(outerSelect, groupAliases...)
	if useLimit {
		outerSelect = append(outerSelect, fmt.Sprintf("dense_rank() OVER (ORDER BY limit_metric DESC, %s) as limit_rank", strings.Join(groupAliases, ", ")))
	}
	fromSb.Select(outerSelect...)
	fromSb.From(fromSb.BuilderAs(innerSb, "inner"))

	fromSb.GroupBy(groupByCols...)

	if useLimit {
		outerSelect := []string{bucketIndexAlias, "sample_factor", minAlias, maxAlias}
		if input.SavedMetricState != nil {
			outerSelect = append(outerSelect, "max_block_number")
		}
		for idx := range input.Expressions {
			outerSelect = append(outerSelect, fmt.Sprintf("metric_value%d", idx))
		}
		outerSelect = append(outerSelect, groupAliases...)

		innerSb := fromSb
		fromSb = sqlbuilder.NewSelectBuilder()
		fromSb.Select(outerSelect...)
		fromSb.From(fromSb.BuilderAs(innerSb, "outer"))
		fromSb.Where(fromSb.LessEqualThan("limit_rank", limitCount))
	}

	fromSb.OrderBy(orderByCols...)
	fromSb.Limit(10000)

	if input.SavedMetricState != nil {
		if err := client.saveMetricHistory(ctx, fromSb, input); err != nil {
			return nil, err
		}
		return nil, nil
	}

	sql, args := fromSb.BuildWithFlavor(sqlbuilder.ClickHouse)

	span, ctx = util.StartSpanFromContext(ctx, "readMetrics.query")
	span.SetAttribute("sql", sql)
	span.SetAttribute("args", args)
	rows, err := client.conn.Query(
		ctx,
		sql,
		args...,
	)
	span.Finish(err)

	if err != nil {
		return nil, err
	}

	var (
		groupKey     uint64
		sampleFactor float64
		min          float64
		max          float64
	)

	groupByColResults := make([]string, len(input.GroupBy))
	metricResults := make([]*float64, len(input.Expressions))
	scanResults := []interface{}{}
	scanResults = append(scanResults, &groupKey)
	scanResults = append(scanResults, &sampleFactor)
	scanResults = append(scanResults, &min)
	scanResults = append(scanResults, &max)
	for idx := range input.Expressions {
		scanResults = append(scanResults, &metricResults[idx])
	}
	for idx := range groupByColResults {
		scanResults = append(scanResults, &groupByColResults[idx])
	}

	metrics := &modelInputs.MetricsBuckets{
		Buckets: []*modelInputs.MetricBucket{},
	}
	lastBucketId := -1
	hasRows := false
	for rows.Next() {
		hasRows = true
		if err := rows.Scan(scanResults...); err != nil {
			return nil, err
		}

		bucketId := groupKey
		if bucketId >= uint64(bucketingInfo.BucketCount) {
			continue
		}

		// Interpolate any missing buckets
		for i := lastBucketId + 1; i < int(bucketId); i++ {
			for _, e := range input.Expressions {
				metrics.Buckets = append(metrics.Buckets, &modelInputs.MetricBucket{
					BucketID:    uint64(i),
					BucketMin:   pointy.Float64(float64(i)*(max-min)/float64(bucketingInfo.BucketCount) + min),
					BucketMax:   pointy.Float64(float64(i+1)*(max-min)/float64(bucketingInfo.BucketCount) + min),
					Group:       append(make([]string, 0), groupByColResults...),
					MetricType:  e.Aggregator,
					Column:      e.Column,
					MetricValue: nil,
				})
			}
		}

		for idx, e := range input.Expressions {
			result := metricResults[idx]
			metrics.Buckets = append(metrics.Buckets, &modelInputs.MetricBucket{
				BucketID:    bucketId,
				BucketMin:   pointy.Float64(float64(bucketId)*(max-min)/float64(bucketingInfo.BucketCount) + min),
				BucketMax:   pointy.Float64(float64(bucketId+1)*(max-min)/float64(bucketingInfo.BucketCount) + min),
				Group:       append(make([]string, 0), groupByColResults...),
				MetricType:  e.Aggregator,
				Column:      e.Column,
				MetricValue: result,
			})
		}

		lastBucketId = int(bucketId)
	}

	// If no rows in the result set, manually set start / end date to interpolate buckets
	if input.BucketBy == modelInputs.MetricBucketByTimestamp.String() && !hasRows {
		min = float64(input.Params.DateRange.StartDate.Unix())
		max = float64(input.Params.DateRange.EndDate.Unix())
	}

	// Interpolate any missing buckets
	for i := lastBucketId + 1; i < bucketingInfo.BucketCount; i++ {
		for _, e := range input.Expressions {
			metrics.Buckets = append(metrics.Buckets, &modelInputs.MetricBucket{
				BucketID:    uint64(i),
				BucketMin:   pointy.Float64(float64(i)*(max-min)/float64(bucketingInfo.BucketCount) + min),
				BucketMax:   pointy.Float64(float64(i+1)*(max-min)/float64(bucketingInfo.BucketCount) + min),
				Group:       append(make([]string, 0), groupByColResults...),
				MetricType:  e.Aggregator,
				Column:      e.Column,
				MetricValue: nil,
			})
		}
	}

	metrics.SampleFactor = sampleFactor
	metrics.BucketCount = uint64(bucketingInfo.BucketCount)

	return metrics, err
}

func formatColumn(input string, column string) string {
	base := input
	if base == "" {
		base = "null"
	}
	return fmt.Sprintf("%s AS %s", base, column)
}

func logLines(ctx context.Context, client *Client, tableConfig model.TableConfig, projectID int, params modelInputs.QueryInput) ([]*modelInputs.LogLine, error) {
	body := formatColumn(tableConfig.BodyColumn, "Body")
	severity := formatColumn(tableConfig.SeverityColumn, "Severity")
	allAttributesColumns := lo.Map(tableConfig.AttributesColumns, func(mapping model.ColumnMapping, _ int) string { return mapping.Column })
	attributes := formatColumn(fmt.Sprintf("mapConcat(%s)", strings.Join(allAttributesColumns, ", ")), "Labels")
	fromSb, _, err := makeSelectBuilder(
		tableConfig,
		[]string{"Timestamp", body, severity, attributes},
		[]int{projectID},
		params,
		Pagination{CountOnly: true},
	)
	if err != nil {
		return nil, err
	}
	fromSb.Limit(1000)

	sql, args := fromSb.BuildWithFlavor(sqlbuilder.ClickHouse)

	logLines := []*modelInputs.LogLine{}

	rows, err := client.conn.Query(
		ctx,
		sql,
		args...,
	)

	if err != nil {
		return nil, err
	}

	for rows.Next() {
		var result struct {
			Timestamp time.Time
			Body      string
			Severity  string
			Labels    map[string]string
		}
		if err := rows.ScanStruct(&result); err != nil {
			return nil, err
		}

		labelsJson, err := json.Marshal(expandJSON(result.Labels))
		if err != nil {
			return nil, err
		}

		var severity *modelInputs.LogLevel
		if result.Severity != "" {
			logLevel := makeLogLevel(result.Severity)
			severity = &logLevel
		}
		logLines = append(logLines, &modelInputs.LogLine{
			Timestamp: result.Timestamp,
			Body:      result.Body,
			Severity:  severity,
			Labels:    string(labelsJson),
		})
	}

	return logLines, err
}

func repr(val reflect.Value) string {
	switch val.Kind() {
	case reflect.Pointer:
		return repr(val.Elem())
	case reflect.Bool:
		return fmt.Sprintf("%t", val.Bool())
	default:
		return val.String()
	}
}

func getSortOrders(
	sb *sqlbuilder.SelectBuilder,
	paginationDirection modelInputs.SortDirection,
	config model.TableConfig,
	params modelInputs.QueryInput,
) (string, string) {
	sortColumn := "timestamp"
	sortDirection := modelInputs.SortDirectionDesc
	if params.Sort != nil {
		sortColumn = params.Sort.Column
		sortDirection = params.Sort.Direction
	}

	if col, found := config.KeysToColumns[sortColumn]; found {
		sortColumn = col
	} else {
		attributesColumn := model.GetAttributesColumn(config.AttributesColumns, sortColumn)
		sortColumn = fmt.Sprintf("%s[%s]", attributesColumn, sb.Var(sortColumn))
	}

	forwardDirection := "DESC"
	backwardDirection := "ASC"
	if paginationDirection == modelInputs.SortDirectionAsc || sortDirection == modelInputs.SortDirectionAsc {
		forwardDirection = "ASC"
		backwardDirection = "DESC"
	}

	orderForward := fmt.Sprintf("%s %s, UUID %s", sortColumn, forwardDirection, forwardDirection)
	orderBackward := fmt.Sprintf("%s %s, UUID %s", sortColumn, backwardDirection, backwardDirection)

	return orderForward, orderBackward
}

func useSamplingTable(params modelInputs.QueryInput) bool {
	// If we have a non-default sort column use the sampling table
	return params.Sort != nil && strings.ToLower(params.Sort.Column) != "timestamp"
}
