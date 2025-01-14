// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License included
// in the file licenses/BSL.txt and at www.mariadb.com/bsl11.
//
// Change Date: 2022-10-01
//
// On the date above, in accordance with the Business Source License, use
// of this software will be governed by the Apache License, Version 2.0,
// included in the file licenses/APL.txt and at
// https://www.apache.org/licenses/LICENSE-2.0

package sql

import (
	"context"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
)

// createViewNode represents a CREATE VIEW statement.
type createViewNode struct {
	n             *tree.CreateView
	dbDesc        *sqlbase.DatabaseDescriptor
	sourceColumns sqlbase.ResultColumns
	// planDeps tracks which tables and views the view being created
	// depends on. This is collected during the construction of
	// the view query's logical plan.
	planDeps planDependencies
}

// CreateView creates a view.
// Privileges: CREATE on database plus SELECT on all the selected columns.
//   notes: postgres requires CREATE on database plus SELECT on all the
//						selected columns.
//          mysql requires CREATE VIEW plus SELECT on all the selected columns.
func (p *planner) CreateView(ctx context.Context, n *tree.CreateView) (planNode, error) {
	dbDesc, err := p.ResolveUncachedDatabase(ctx, &n.Name)
	if err != nil {
		return nil, err
	}

	if err := p.CheckPrivilege(ctx, dbDesc, privilege.CREATE); err != nil {
		return nil, err
	}

	var planDeps planDependencies
	var sourceColumns sqlbase.ResultColumns
	// To avoid races with ongoing schema changes to tables that the view
	// depends on, make sure we use the most recent versions of table
	// descriptors rather than the copies in the lease cache.
	p.runWithOptions(resolveFlags{skipCache: true}, func() {
		planDeps, sourceColumns, err = p.analyzeViewQuery(ctx, n.AsSource)
	})
	if err != nil {
		return nil, err
	}

	// Ensure that all the table names pretty-print as fully qualified,
	// so we store that in the view descriptor.
	//
	// The traversal will update the TableNames in-place, so the changes are
	// persisted in n.AsSource. We exploit the fact that semantic analysis above
	// has populated any missing db/schema details in the table names in-place.
	// We use tree.FormatNode merely as a traversal method; its output buffer is
	// discarded immediately after the traversal because it is not needed further.
	{
		f := tree.NewFmtCtx(tree.FmtParsable)
		f.SetReformatTableNames(
			func(_ *tree.FmtCtx, tn *tree.TableName) {
				// Persist the database prefix expansion.
				if tn.SchemaName != "" {
					// All CTE or table aliases have no schema
					// information. Those do not turn into explicit.
					tn.ExplicitSchema = true
					tn.ExplicitCatalog = true
				}
			},
		)
		f.FormatNode(n.AsSource)
		f.Close() // We don't need the string.
	}

	numColNames := len(n.ColumnNames)
	numColumns := len(sourceColumns)
	if numColNames != 0 && numColNames != numColumns {
		return nil, sqlbase.NewSyntaxError(fmt.Sprintf(
			"CREATE VIEW specifies %d column name%s, but data source has %d column%s",
			numColNames, util.Pluralize(int64(numColNames)),
			numColumns, util.Pluralize(int64(numColumns))))
	}

	log.VEventf(ctx, 2, "collected view dependencies:\n%s", planDeps.String())

	return &createViewNode{
		n:             n,
		dbDesc:        dbDesc,
		sourceColumns: sourceColumns,
		planDeps:      planDeps,
	}, nil
}

func (n *createViewNode) startExec(params runParams) error {
	viewName := n.n.Name.Table()
	tKey := sqlbase.NewTableKey(n.dbDesc.ID, viewName)
	key := tKey.Key()
	if exists, err := descExists(params.ctx, params.p.txn, key); err == nil && exists {
		// TODO(a-robinson): Support CREATE OR REPLACE commands.
		return sqlbase.NewRelationAlreadyExistsError(tKey.Name())
	} else if err != nil {
		return err
	}

	id, err := GenerateUniqueDescID(params.ctx, params.extendedEvalCtx.ExecCfg.DB)
	if err != nil {
		return err
	}

	// Inherit permissions from the database descriptor.
	privs := n.dbDesc.GetPrivileges()

	desc, err := n.makeViewTableDesc(
		params,
		viewName,
		n.n.ColumnNames,
		n.dbDesc.ID,
		id,
		n.sourceColumns,
		privs,
	)
	if err != nil {
		return err
	}

	// Collect all the tables/views this view depends on.
	for backrefID := range n.planDeps {
		desc.DependsOn = append(desc.DependsOn, backrefID)
	}

	if err = params.p.createDescriptorWithID(
		params.ctx, key, id, &desc, params.EvalContext().Settings); err != nil {
		return err
	}

	// Persist the back-references in all referenced table descriptors.
	for _, updated := range n.planDeps {
		backrefID := updated.desc.ID
		backRefMutable := params.p.Tables().getUncommittedTableByID(backrefID).MutableTableDescriptor
		if backRefMutable == nil {
			backRefMutable = sqlbase.NewMutableExistingTableDescriptor(*updated.desc.TableDesc())
		}
		for _, dep := range updated.deps {
			// The logical plan constructor merely registered the dependencies.
			// It did not populate the "ID" field of TableDescriptor_Reference,
			// because the ID of the newly created view descriptor was not
			// yet known.
			// We need to do it here.
			dep.ID = desc.ID
			backRefMutable.DependedOnBy = append(backRefMutable.DependedOnBy, dep)
		}
		if err := params.p.writeSchemaChange(params.ctx, backRefMutable, sqlbase.InvalidMutationID); err != nil {
			return err
		}
	}

	if err := desc.Validate(params.ctx, params.p.txn, params.EvalContext().Settings); err != nil {
		return err
	}

	// Log Create View event. This is an auditable log event and is
	// recorded in the same transaction as the table descriptor update.
	return MakeEventLogger(params.extendedEvalCtx.ExecCfg).InsertEventRecord(
		params.ctx,
		params.p.txn,
		EventLogCreateView,
		int32(desc.ID),
		int32(params.extendedEvalCtx.NodeID),
		struct {
			ViewName  string
			Statement string
			User      string
		}{n.n.Name.FQString(), n.n.String(), params.SessionData().User},
	)
}

func (*createViewNode) Next(runParams) (bool, error) { return false, nil }
func (*createViewNode) Values() tree.Datums          { return tree.Datums{} }
func (n *createViewNode) Close(ctx context.Context)  {}

// makeViewTableDesc returns the table descriptor for a new view.
//
// It creates the descriptor directly in the PUBLIC state rather than
// the ADDING state because back-references are added to the view's
// dependencies in the same transaction that the view is created and it
// doesn't matter if reads/writes use a cached descriptor that doesn't
// include the back-references.
func (n *createViewNode) makeViewTableDesc(
	params runParams,
	viewName string,
	columnNames tree.NameList,
	parentID sqlbase.ID,
	id sqlbase.ID,
	resultColumns []sqlbase.ResultColumn,
	privileges *sqlbase.PrivilegeDescriptor,
) (sqlbase.MutableTableDescriptor, error) {
	desc := InitTableDescriptor(id, parentID, viewName,
		params.p.txn.CommitTimestamp(), privileges)
	desc.ViewQuery = tree.AsStringWithFlags(n.n.AsSource, tree.FmtParsable)
	for i, colRes := range resultColumns {
		columnTableDef := tree.ColumnTableDef{Name: tree.Name(colRes.Name), Type: colRes.Typ}
		if len(columnNames) > i {
			columnTableDef.Name = columnNames[i]
		}
		// The new types in the CREATE VIEW column specs never use
		// SERIAL so we need not process SERIAL types here.
		col, _, _, err := sqlbase.MakeColumnDefDescs(&columnTableDef, &params.p.semaCtx)
		if err != nil {
			return desc, err
		}
		desc.AddColumn(col)
	}
	// AllocateIDs mutates its receiver. `return desc, desc.AllocateIDs()`
	// happens to work in gc, but does not work in gccgo.
	//
	// See https://github.com/golang/go/issues/23188.
	err := desc.AllocateIDs()
	return desc, err
}

// MakeViewTableDesc returns the table descriptor for a new view.
func MakeViewTableDesc(
	n *tree.CreateView,
	resultColumns sqlbase.ResultColumns,
	parentID, id sqlbase.ID,
	creationTime hlc.Timestamp,
	privileges *sqlbase.PrivilegeDescriptor,
	semaCtx *tree.SemaContext,
	evalCtx *tree.EvalContext,
) (sqlbase.MutableTableDescriptor, error) {
	viewName := n.Name.Table()
	desc := InitTableDescriptor(id, parentID, viewName, creationTime, privileges)
	desc.ViewQuery = tree.AsStringWithFlags(n.AsSource, tree.FmtParsable)

	for i, colRes := range resultColumns {
		columnTableDef := tree.ColumnTableDef{Name: tree.Name(colRes.Name), Type: colRes.Typ}
		if len(n.ColumnNames) > i {
			columnTableDef.Name = n.ColumnNames[i]
		}
		// The new types in the CREATE VIEW column specs never use
		// SERIAL so we need not process SERIAL types here.
		col, _, _, err := sqlbase.MakeColumnDefDescs(&columnTableDef, semaCtx)
		if err != nil {
			return desc, err
		}
		desc.AddColumn(col)
	}
	// AllocateIDs mutates its receiver. `return desc, desc.AllocateIDs()`
	// happens to work in gc, but does not work in gccgo.
	//
	// See https://github.com/golang/go/issues/23188.
	err := desc.AllocateIDs()
	return desc, err
}
