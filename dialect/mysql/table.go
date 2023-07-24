package mysql

import (
	"context"
	"fmt"
	"reflect"

	"github.com/stephenafamo/bob"
	"github.com/stephenafamo/bob/dialect/mysql/dialect"
	"github.com/stephenafamo/bob/dialect/mysql/dm"
	"github.com/stephenafamo/bob/dialect/mysql/im"
	"github.com/stephenafamo/bob/dialect/mysql/sm"
	"github.com/stephenafamo/bob/dialect/mysql/um"
	"github.com/stephenafamo/bob/internal"
	"github.com/stephenafamo/bob/orm"
)

func NewTable[T any, Tset any](tableName string, uniques ...[]string) *Table[T, []T, Tset] {
	return NewTablex[T, []T, Tset](tableName, uniques...)
}

func NewTablex[T any, Tslice ~[]T, Tset any](tableName string, uniques ...[]string) *Table[T, Tslice, Tset] {
	var zeroSet Tset

	setMapping := internal.GetMappings(reflect.TypeOf(zeroSet))

	view, mappings := newView[T, Tslice](tableName)
	t := &Table[T, Tslice, Tset]{
		View:       view,
		pkCols:     internal.FilterNonZero(mappings.PKs),
		setMapping: setMapping,
	}

	allAutoIncr := internal.FilterNonZero(mappings.AutoIncrement)
	if len(allAutoIncr) == 1 {
		setAutoIncr := internal.FilterNonZero(setMapping.AutoIncrement)
		if len(allAutoIncr) == len(setAutoIncr) && allAutoIncr[0] == setAutoIncr[0] {
			t.autoIncrementColumn = allAutoIncr[0]
			return t
		}
	}

	// Do this only if needed
	if t.autoIncrementColumn == "" {
		t.uniqueIdx = uniqueIndexes(setMapping.All, uniques...)
	}

	t.unretrievable = t.autoIncrementColumn == "" && len(t.uniqueIdx) == 0

	return t
}

// The table contains extract information from the struct and contains
// caches ???
type Table[T any, Tslice ~[]T, Tset any] struct {
	*View[T, Tslice]
	pkCols     []string
	setMapping internal.Mapping

	BeforeInsertHooks orm.Hooks[[]Tset]
	BeforeUpsertHooks orm.Hooks[[]Tset]

	// NOTE: This is not called by InsertMany()
	AfterInsertOneHooks orm.Hooks[T]
	// NOTE: This is not called by UpsertMany()
	AfterUpsertOneHooks orm.Hooks[T]

	BeforeUpdateHooks orm.Hooks[Tslice]
	AfterUpdateHooks  orm.Hooks[Tslice]

	BeforeDeleteHooks orm.Hooks[Tslice]
	AfterDeleteHooks  orm.Hooks[Tslice]

	// The AUTO_INCREMENT column that we can use to retrieve values using lastInsertID
	// If empty, there is no auto inc
	autoIncrementColumn string

	// field indexes of unique columns
	uniqueIdx [][]int

	// save if we can retrieve or not
	unretrievable bool
}

// Insert inserts a row into the table with only the set columns in Tset
//   - If the table has an AUTO_INCREMENT column,
//     the inserted row is retrieved using the lastInsertID
//   - If there is no AUTO_INCREMENT but the table has a unique indes that
//     has all columns set in the setional row, then the values of the unique columns
//     are used to retrieve the inserted row
//
// If there is none of the above methods are possible, a zero value and
// [ErrCannotRetrieveRow] is returned after a successful insert
func (t *Table[T, Tslice, Tset]) Insert(ctx context.Context, exec bob.Executor, row Tset) (T, error) {
	var err error
	var zero T

	ctx, err = t.BeforeInsertHooks.Do(ctx, exec, []Tset{row})
	if err != nil {
		return zero, err
	}

	columns, values, err := internal.GetColumnValues(t.setMapping.NonGenerated, nil, row)
	if err != nil {
		return zero, fmt.Errorf("get insert values: %w", err)
	}

	q := Insert(
		im.Into(t.Name(ctx), columns...),
		im.Rows(values...),
	)

	result, err := q.Exec(ctx, exec)
	if err != nil {
		return zero, err
	}

	if t.unretrievable {
		return zero, orm.ErrCannotRetrieveRow
	}

	if t.autoIncrementColumn != "" {
		lastID, err := result.LastInsertId()
		if err != nil {
			return zero, err
		}

		return t.Query(ctx, exec, sm.Where(Quote(t.autoIncrementColumn).EQ(Arg(lastID)))).One()
	}

	uCols, uArgs := t.uniqueSet(row)
	if len(uCols) == 0 {
		return zero, orm.ErrCannotRetrieveRow
	}

	q2 := t.Query(ctx, exec)
	for i := range uCols {
		sm.Where(Quote(uCols[i]).EQ(Arg(uArgs[i]))).Apply(q2.Expression)
	}

	val, err := q2.One()
	if err != nil {
		return zero, err
	}

	_, err = t.AfterInsertOneHooks.Do(ctx, exec, val)
	if err != nil {
		return val, err
	}

	return val, nil
}

// InsertMany inserts multiple row into the table with only the set columns in Tset
// and returns the number of inserted rows
func (t *Table[T, Tslice, Tset]) InsertMany(ctx context.Context, exec bob.Executor, rows ...Tset) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	var err error

	ctx, err = t.BeforeInsertHooks.Do(ctx, exec, rows)
	if err != nil {
		return 0, err
	}

	columns, values, err := internal.GetColumnValues(t.setMapping.NonGenerated, nil, rows...)
	if err != nil {
		return 0, fmt.Errorf("get insert values: %w", err)
	}

	// If there are no columns, force at least one column with "DEFAULT" for each row
	if len(columns) == 0 {
		columns = []string{internal.FirstNonEmpty(t.setMapping.All)}
		values = make([][]bob.Expression, len(rows))
		for i := range rows {
			values[i] = []bob.Expression{Raw("DEFAULT")}
		}
	}

	q := Insert(
		im.Into(t.Name(ctx), columns...),
		im.Rows(values...),
	)

	result, err := q.Exec(ctx, exec)
	if err != nil {
		return 0, err
	}

	return result.RowsAffected()
}

// Updates the given model
// if columns is nil, every non-primary-key column is updated
// NOTE: values from the DB are not refreshed into the model
func (t *Table[T, Tslice, Tset]) Update(ctx context.Context, exec bob.Executor, row T, cols ...string) (int64, error) {
	_, err := t.BeforeUpdateHooks.Do(ctx, exec, Tslice{row})
	if err != nil {
		return 0, err
	}

	q := Update(um.Table(t.NameAs(ctx)))

	pks, pkVals, err := internal.GetColumnValues(t.mapping.PKs, t.mapping.PKs, row)
	if err != nil {
		return 0, fmt.Errorf("get update pk values: %w", err)
	}

	if len(cols) == 0 {
		cols = t.mapping.NonPKs
	}
	columns, values, err := internal.GetColumnValues(t.mapping.NonGenerated, cols, row)
	if err != nil {
		return 0, fmt.Errorf("get update values: %w", err)
	}

	for i, pk := range pks {
		q.Apply(um.Where(Quote(pk).EQ(pkVals[0][i])))
	}

	for i, col := range columns {
		q.Apply(um.Set(col).To(values[0][i]))
	}

	result, err := q.Exec(ctx, exec)
	if err != nil {
		return 0, err
	}

	_, err = t.AfterUpdateHooks.Do(ctx, exec, Tslice{row})
	if err != nil {
		return 0, err
	}

	return result.RowsAffected()
}

// Updates the given models
// if columns is nil, every column is updated
// NOTE: values from the DB are not refreshed into the models
func (t *Table[T, Tslice, Tset]) UpdateMany(ctx context.Context, exec bob.Executor, vals Tset, rows ...T) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	columns, values, err := internal.GetColumnValues(t.setMapping.NonGenerated, nil, vals)
	if err != nil {
		return 0, fmt.Errorf("get upsert values: %w", err)
	}
	if len(columns) == 0 {
		return 0, orm.ErrNothingToUpdate
	}

	_, err = t.BeforeUpdateHooks.Do(ctx, exec, rows)
	if err != nil {
		return 0, err
	}

	q := Update(um.Table(t.NameAs(ctx)))

	for i, col := range columns {
		q.Apply(um.Set(col).To(values[0][i]))
	}

	// Find a set the PKs
	pks, pkVals, err := internal.GetColumnValues(t.mapping.PKs, t.mapping.PKs, rows...)
	if err != nil {
		return 0, fmt.Errorf("get update pk values: %w", err)
	}

	if len(pks) == 1 {
		pkValues := make([]bob.Expression, len(pkVals))
		for i, pair := range pkVals {
			pkValues[i] = pair[0]
		}

		q.Apply(um.Where(
			Quote(pks[0]).In(pkValues...),
		))
	} else {
		pkPairs := make([]bob.Expression, len(pkVals))
		for i, pair := range pkVals {
			pkPairs[i] = Group(pair...)
		}

		pkGroup := make([]bob.Expression, len(pks))
		for i, pk := range pks {
			pkGroup[i] = Quote(pk)
		}

		q.Apply(um.Where(
			Group(pkGroup...).In(pkPairs...),
		))
	}

	result, err := q.Exec(ctx, exec)
	if err != nil {
		return 0, err
	}

	_, err = t.AfterUpdateHooks.Do(ctx, exec, rows)
	if err != nil {
		return 0, err
	}

	return result.RowsAffected()
}

// Uses the setional columns to know what to insert
// If updateCols is nil, it updates all the columns set in Tset
func (t *Table[T, Tslice, Tset]) Upsert(ctx context.Context, exec bob.Executor, updateOnConflict bool, updateCols []string, row Tset) (T, error) {
	var err error
	var zero T

	ctx, err = t.BeforeUpsertHooks.Do(ctx, exec, []Tset{row})
	if err != nil {
		return zero, err
	}

	columns, values, err := internal.GetColumnValues(t.setMapping.NonGenerated, nil, row)
	if err != nil {
		return zero, fmt.Errorf("get upsert values: %w", err)
	}

	var conflictQM bob.Mod[*dialect.InsertQuery]
	if !updateOnConflict {
		conflictQM = im.Ignore()
	} else {
		if len(updateCols) == 0 {
			updateCols = columns
		}

		conflictQM = im.OnDuplicateKeyUpdate().SetValues(updateCols...)
	}

	q := Insert(
		im.Into(t.Name(ctx), columns...),
		im.Rows(values...),
		im.As(t.alias),
		conflictQM,
	)

	result, err := bob.Exec(ctx, exec, q)
	if err != nil {
		return zero, err
	}

	if t.unretrievable {
		return zero, orm.ErrCannotRetrieveRow
	}

	if t.autoIncrementColumn != "" {
		lastID, err := result.LastInsertId()
		if err != nil {
			return zero, err
		}

		return t.Query(ctx, exec, sm.Where(Quote(t.autoIncrementColumn).EQ(Arg(lastID)))).One()
	}

	uCols, uArgs := t.uniqueSet(row)
	if len(uCols) == 0 {
		return zero, orm.ErrCannotRetrieveRow
	}

	q2 := t.Query(ctx, exec)
	for i := range uCols {
		sm.Where(Quote(uCols[i]).EQ(Arg(uArgs[i]))).Apply(q2.Expression)
	}

	val, err := q2.One()
	if err != nil {
		return zero, err
	}

	_, err = t.AfterUpsertOneHooks.Do(ctx, exec, val)
	if err != nil {
		return val, err
	}

	return val, nil
}

// Uses the setional columns to know what to insert
// If updateCols is nil, it updates all the columns set in Tset
func (t *Table[T, Tslice, Tset]) UpsertMany(ctx context.Context, exec bob.Executor, updateOnConflict bool, updateCols []string, rows ...Tset) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	var err error

	ctx, err = t.BeforeUpsertHooks.Do(ctx, exec, rows)
	if err != nil {
		return 0, err
	}

	columns, values, err := internal.GetColumnValues(t.setMapping.NonGenerated, nil, rows...)
	if err != nil {
		return 0, fmt.Errorf("get upsert values: %w", err)
	}

	// If there are no columns, force at least one column with "DEFAULT" for each row
	if len(columns) == 0 {
		columns = []string{internal.FirstNonEmpty(t.setMapping.All)}
		values = make([][]bob.Expression, len(rows))
		for i := range rows {
			values[i] = []bob.Expression{Raw("DEFAULT")}
		}
	}

	var conflictQM bob.Mod[*dialect.InsertQuery]
	if !updateOnConflict {
		conflictQM = im.Ignore()
	} else {
		if len(updateCols) == 0 {
			updateCols = columns
		}

		conflictQM = im.OnDuplicateKeyUpdate().SetValues(updateCols...)
	}

	q := Insert(
		im.Into(t.Name(ctx), columns...),
		conflictQM,
	)

	for _, val := range values {
		q.Apply(im.Values(val...))
	}

	result, err := q.Exec(ctx, exec)
	if err != nil {
		return 0, err
	}

	return result.RowsAffected()
}

// Deletes the given model
// if columns is nil, every column is deleted
func (t *Table[T, Tslice, Tset]) Delete(ctx context.Context, exec bob.Executor, row T) (int64, error) {
	_, err := t.BeforeDeleteHooks.Do(ctx, exec, Tslice{row})
	if err != nil {
		return 0, err
	}

	q := Delete(dm.From(t.NameAs(ctx)))

	pks, pkVals, err := internal.GetColumnValues(t.mapping.PKs, t.mapping.PKs, row)
	if err != nil {
		return 0, fmt.Errorf("get update pk values: %w", err)
	}

	for i, pk := range pks {
		q.Apply(dm.Where(Quote(pk).EQ(pkVals[0][i])))
	}

	result, err := q.Exec(ctx, exec)
	if err != nil {
		return 0, err
	}

	_, err = t.AfterDeleteHooks.Do(ctx, exec, Tslice{row})
	if err != nil {
		return 0, err
	}

	return result.RowsAffected()
}

// Deletes the given models
// if columns is nil, every column is deleted
func (t *Table[T, Tslice, Tset]) DeleteMany(ctx context.Context, exec bob.Executor, rows ...T) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	_, err := t.BeforeDeleteHooks.Do(ctx, exec, rows)
	if err != nil {
		return 0, err
	}

	q := Delete(dm.From(t.NameAs(ctx)))

	// Find a set the PKs
	pks, pkVals, err := internal.GetColumnValues(t.mapping.PKs, t.mapping.PKs, rows...)
	if err != nil {
		return 0, fmt.Errorf("get update pk values: %w", err)
	}

	if len(pks) == 1 {
		pkValues := make([]bob.Expression, len(pkVals))
		for i, pair := range pkVals {
			pkValues[i] = pair[0]
		}

		q.Apply(dm.Where(
			Quote(pks[0]).In(pkValues...),
		))
	} else {
		pkPairs := make([]bob.Expression, len(pkVals))
		for i, pair := range pkVals {
			pkPairs[i] = Group(pair...)
		}

		pkGroup := make([]bob.Expression, len(pks))
		for i, pk := range pks {
			pkGroup[i] = Quote(pk)
		}

		q.Apply(dm.Where(
			Group(pkGroup...).In(pkPairs...),
		))
	}

	result, err := q.Exec(ctx, exec)
	if err != nil {
		return 0, err
	}

	_, err = t.AfterDeleteHooks.Do(ctx, exec, rows)
	if err != nil {
		return 0, err
	}

	return result.RowsAffected()
}

func uniqueIndexes(allCols []string, uniques ...[]string) [][]int {
	var indexes [][]int
	for _, unique := range uniques {
		index := make([]int, 0, len(unique))
		for _, name := range unique {
			for i, col := range allCols {
				if name == col {
					index = append(index, i)
				}
			}
		}

		// all columns found
		if len(index) == len(unique) {
			indexes = append(indexes, index)
		}
	}

	return indexes
}

//nolint:gochecknoglobals
var settableTyp = reflect.TypeOf((*interface{ IsSet() bool })(nil)).Elem()

func (t *Table[T, Tslice, Tset]) uniqueSet(row Tset) ([]string, []any) {
	val := reflect.ValueOf(row)
	if val.Kind() == reflect.Pointer {
		if val.IsNil() {
			return nil, nil
		}
		val = val.Elem()
	}

	for _, u := range t.uniqueIdx {
		colNames := make([]string, 0, len(u))
		args := make([]any, 0, len(u))
		for _, col := range u {
			field := val.Field(col)

			// If it does not implement the type, break
			if !field.Type().Implements(settableTyp) {
				break
			}

			// if it is not set break
			if !field.MethodByName("IsSet").Call(nil)[0].Interface().(bool) {
				break
			}

			colNames = append(colNames, t.setMapping.All[col])
			args = append(args, field.Interface())
		}

		if len(colNames) == len(u) {
			return colNames, args
		}
	}

	return nil, nil
}

// Starts an update query for this table
func (t *Table[T, Tslice, Tset]) UpdateAll(ctx context.Context, exec bob.Executor, queryMods ...bob.Mod[*dialect.UpdateQuery]) *TQuery[*dialect.UpdateQuery, T, Tslice] {
	q := &TQuery[*dialect.UpdateQuery, T, Tslice]{
		BaseQuery: Update(um.Table(t.NameAs(ctx))),
		ctx:       ctx,
		exec:      exec,
		view:      t.View,
	}

	// q.Expression.SetLoadContext(ctx)
	q.Apply(queryMods...)

	return q
}

// Starts a delete query for this table
func (t *Table[T, Tslice, Tset]) DeleteAll(ctx context.Context, exec bob.Executor, queryMods ...bob.Mod[*dialect.DeleteQuery]) *TQuery[*dialect.DeleteQuery, T, Tslice] {
	q := &TQuery[*dialect.DeleteQuery, T, Tslice]{
		BaseQuery: Delete(dm.From(t.NameAs(ctx))),
		ctx:       ctx,
		exec:      exec,
		view:      t.View,
	}

	// q.Expression.SetLoadContext(ctx)
	q.Apply(queryMods...)

	return q
}

type TQuery[Q bob.Expression, T any, Ts ~[]T] struct {
	bob.BaseQuery[Q]
	ctx  context.Context
	exec bob.Executor
	view *View[T, Ts]
}

// Execute the query
func (t *TQuery[Q, T, Tslice]) Exec() (int64, error) {
	result, err := t.BaseQuery.Exec(t.ctx, t.exec)
	if err != nil {
		return 0, err
	}

	return result.RowsAffected()
}
