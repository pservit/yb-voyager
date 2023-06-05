package cmd

import (
	"github.com/tebeka/atexit"
	"github.com/vbauerster/mpb/v8"
	"github.com/yugabyte/yb-voyager/yb-voyager/src/dbzm"
	"github.com/yugabyte/yb-voyager/yb-voyager/src/pbreporter"
)

type ProgressTracker struct {
	totalRowCount map[string]int64

	inProgressTableSno           int
	inProgressQualifiedTableName string

	mpbProgress *mpb.Progress
	pb          pbreporter.ExportProgressReporter
}

func NewProgressTracker(totalRowCount map[string]int64) *ProgressTracker {
	pt := &ProgressTracker{
		totalRowCount: totalRowCount,
		mpbProgress:   mpb.New(),
	}
	atexit.Register(func() {
		pt.mpbProgress.Shutdown()
	})
	return pt
}

func (pt *ProgressTracker) UpdateProgress(status *dbzm.ExportStatus) {
	if status == nil || status.InProgressTableSno() == -1 {
		return
	}

	inProgressTableSno := status.InProgressTableSno()
	if pt.pb == nil || pt.inProgressTableSno != inProgressTableSno {
		// Complete currently in-progress progress-bar.
		if pt.pb != nil {
			pt.pb.SetTotalRowCount(pt.totalRowCount[pt.inProgressQualifiedTableName], true)
			pt.pb = nil
		}
		// Start new progress-bar.
		pt.inProgressTableSno = inProgressTableSno
		pt.inProgressQualifiedTableName = status.GetQualifiedTableName(pt.inProgressTableSno)
		pt.pb = pbreporter.NewExportPB(pt.mpbProgress, pt.inProgressQualifiedTableName, disablePb)
		pt.pb.SetTotalRowCount(pt.totalRowCount[pt.inProgressQualifiedTableName], false)
	}
	exportedRowCount := status.GetTableExportedRowCount(pt.inProgressTableSno)
	if pt.totalRowCount[pt.inProgressQualifiedTableName] <= exportedRowCount {
		pt.totalRowCount[pt.inProgressQualifiedTableName] = int64(float64(exportedRowCount) * 1.05)
		pt.pb.SetTotalRowCount(pt.totalRowCount[pt.inProgressQualifiedTableName], false)
	}
	pt.pb.SetExportedRowCount(exportedRowCount)
}

func (pt *ProgressTracker) Done(status *dbzm.ExportStatus) {
	if pt.pb != nil {
		exportedRowCount := status.GetTableExportedRowCount(pt.inProgressTableSno)
		pt.pb.SetTotalRowCount(exportedRowCount, true /* Mark complete */)
		pt.pb = nil
	}
	pt.mpbProgress.Wait()
}
