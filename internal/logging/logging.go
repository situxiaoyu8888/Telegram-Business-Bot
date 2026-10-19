// Package logging builds the application logger: console output plus an optional rotated JSON file.
package logging

import (
	"io"
	"os"
	"path/filepath"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

type Options struct {
	Dev        bool
	Dir        string // empty disables file output
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
}

// New returns the logger and the file writer (nil when file output is disabled).
func New(o Options) (*zap.Logger, io.WriteCloser, error) {
	level := zap.NewAtomicLevelAt(zap.InfoLevel)
	consoleCfg := zap.NewProductionEncoderConfig()
	var console zapcore.Encoder
	if o.Dev {
		level.SetLevel(zap.DebugLevel)
		consoleCfg = zap.NewDevelopmentEncoderConfig()
		console = zapcore.NewConsoleEncoder(consoleCfg)
	} else {
		consoleCfg.EncodeTime = zapcore.ISO8601TimeEncoder
		console = zapcore.NewJSONEncoder(consoleCfg)
	}
	cores := []zapcore.Core{zapcore.NewCore(console, zapcore.Lock(os.Stderr), level)}

	var file *lumberjack.Logger
	if o.Dir != "" {
		if err := os.MkdirAll(o.Dir, 0o755); err != nil {
			return nil, nil, err
		}
		file = &lumberjack.Logger{
			Filename:   filepath.Join(o.Dir, "bot.log"),
			MaxSize:    o.MaxSizeMB,
			MaxBackups: o.MaxBackups,
			MaxAge:     o.MaxAgeDays,
			LocalTime:  true,
			Compress:   true,
		}
		fileCfg := zap.NewProductionEncoderConfig()
		fileCfg.EncodeTime = zapcore.ISO8601TimeEncoder
		cores = append(cores, zapcore.NewCore(zapcore.NewJSONEncoder(fileCfg), zapcore.AddSync(file), level))
	}
	opts := []zap.Option{zap.AddCaller(), zap.AddStacktrace(zap.ErrorLevel)}
	if o.Dev {
		opts = append(opts, zap.Development())
	}
	logger := zap.New(zapcore.NewTee(cores...), opts...)
	if file == nil {
		return logger, nil, nil
	}
	return logger, file, nil
}
