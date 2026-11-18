package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/hasankaradmir/karga-sdk/api"
)

func newStateCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "state",
		Short: "Manage karga's state database",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "backup <file>",
		Short: "Write a consistent copy of the state database (safe while karga runs)",
		Long: "Write a consistent copy of the server's state database to <file> (mode 0600, must not exist).\n" +
			"To restore, stop karga-server and copy the file over karga.db in its KARGA_HOME; proxy and\n" +
			"gateway configuration is rebuilt from the state on the next upgrade.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := a.backend()
			if err := writeNew(args[0], func(w io.Writer) error { return s.BackupState(cmd.Context(), w) }); err != nil {
				return err
			}
			return a.report(map[string]any{"file": args[0]}, "state written to %s\n"+
				"The backup does not hold state.key and gateway.json from the server's KARGA_HOME (default\n"+
				"/var/lib/karga): copy them too and keep them safe. Restored without state.key, each release\n"+
				"applies its Secrets once more on its next upgrade.\n", args[0])
		},
	})
	return cmd
}

func newVolumeCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{Use: "volume", Short: "Back up and restore a release's volumes"}
	var backup api.VolumeBackupRequest
	b := &cobra.Command{
		Use:   "backup <release> <volume> <file.tar.gz>",
		Short: "Copy a volume's files to a backup file (0600, must not exist)",
		Long: "Copy a volume's files to a gzipped tar through a workload that mounts it. By default the copy is\n" +
			"taken while the workloads run; --stop stops them meanwhile for a consistent copy. For databases\n" +
			"prefer --stop or the database's own dump tool.",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			backup.Release, backup.Volume = args[0], args[1]
			s := a.backend()
			err := writeNew(args[2], func(w io.Writer) error {
				return s.BackupVolume(cmd.Context(), backup, w)
			})
			if err != nil {
				return err
			}
			return a.report(map[string]any{"release": backup.Release, "volume": backup.Volume, "file": args[2]},
				"volume %s of %s written to %s\n", backup.Volume, backup.Release, args[2])
		},
	}
	b.Flags().BoolVar(&backup.Stop, "stop", false, "stop the workloads mounting the volume during the copy")
	var restore api.VolumeRestoreRequest
	r := &cobra.Command{
		Use:   "restore <release> <volume> <file.tar.gz>",
		Short: "Write a backup into a volume (its workloads are stopped meanwhile)",
		Long: "Write a backup into a volume. The workloads mounting it are stopped during the copy and started\n" +
			"again. Files in the backup overwrite files of the same name; other files in the volume are kept.",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			restore.Release, restore.Volume, restore.Name = args[0], args[1], filepath.Base(args[2])
			what := fmt.Sprintf("This stops the workloads mounting volume %s of %s and overwrites its files with %s", restore.Volume, restore.Release, args[2])
			if err := a.confirm(cmd, what); err != nil {
				return err
			}
			f, err := os.Open(args[2])
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			s := a.backend()
			if err := s.RestoreVolume(cmd.Context(), restore, f); err != nil {
				return err
			}
			return a.report(map[string]any{"release": restore.Release, "volume": restore.Volume, "file": args[2]},
				"volume %s of %s restored from %s\n", restore.Volume, restore.Release, args[2])
		},
	}
	r.Flags().BoolVar(&restore.Force, "from-other-volume", false, "restore a backup taken from another volume or release")
	cmd.AddCommand(b, r)
	return cmd
}

// writeNew creates path (0600, must not exist) and fills it with fn; on failure the file is removed.
func writeNew(path string, fn func(io.Writer) error) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	return fn(f)
}
