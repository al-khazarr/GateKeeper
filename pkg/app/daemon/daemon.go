package daemon

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	_cfg "github.com/al-khazarr/GateKeeper/pkg/app/config"
	_err "github.com/al-khazarr/GateKeeper/pkg/common/error"
	_httplog "github.com/al-khazarr/GateKeeper/pkg/common/httplog"
	_httpserver "github.com/al-khazarr/GateKeeper/pkg/common/httpserver"
	_http "github.com/al-khazarr/GateKeeper/pkg/common/httpservice"
	_log "github.com/al-khazarr/GateKeeper/pkg/common/logger"
	_wpservice "github.com/al-khazarr/GateKeeper/pkg/common/workerpoolservice"

	httphandler "github.com/al-khazarr/GateKeeper/pkg/app/httphandler"
)

// Daemon represent top level daemon
type Daemon struct {
	ctx    context.Context    // корневой контекст
	cancel context.CancelFunc // функция закрытия корневого контекста
	cfg    *_cfg.Config       // конфигурация демона

	// Сервисы демона
	httpServer      *_httpserver.Server // HTTP сервер
	httpServerErrCh chan error          // канал ошибок для HTTP сервера

	httpLogger      *_httplog.Logger // сервис логирования HTTP трафика
	httpLoggerErrCh chan error       // канал ошибок для HTTP логгера

	httpService      *_http.Service // сервис HTTP запросов
	httpServiceErrCh chan error     // канал ошибок для HTTP

	httpHandler      *httphandler.Service // сервис обработки HTTP запросов
	httpHandlerErrCh chan error           // канал ошибок для HTTP

	wpService      *_wpservice.Service // сервис worker pool
	wpServiceErrCh chan error          // канал ошибок для сервиса worker pool
}

// New create Daemon
func New(ctx context.Context, cfg *_cfg.Config) (*Daemon, error) {
	var err error

	_log.Info("Create new daemon")

	{ // входные проверки
		if cfg == nil {
			return nil, _err.NewTyped(_err.ERR_INCORRECT_CALL_ERROR, _err.ERR_UNDEFINED_ID, "if cfg == nil {}").PrintfError()
		}
	} // входные проверки

	// Создаем новый демон
	daemon := &Daemon{
		cfg:              cfg,
		httpServerErrCh:  make(chan error, 1),
		httpServiceErrCh: make(chan error, 1),
		httpHandlerErrCh: make(chan error, 1),
		httpLoggerErrCh:  make(chan error, 1),
		wpServiceErrCh:   make(chan error, 1),
	}

	// создаем корневой контекст с отменой
	if ctx == nil {
		daemon.ctx, daemon.cancel = context.WithCancel(context.Background())
	} else {
		daemon.ctx, daemon.cancel = context.WithCancel(ctx)
	}

	// создаем сервис обработчиков
	if daemon.wpService, err = _wpservice.New(daemon.ctx, "WorkerPool - background", daemon.wpServiceErrCh, &daemon.cfg.WorkerPoolServiceCfg); err != nil {
		return nil, err
	}

	// создаем обработчик для логирования HTTP
	if daemon.httpLogger, err = _httplog.New(daemon.ctx, &daemon.cfg.HttpLoggerCfg); err != nil {
		return nil, err
	}

	// HTTP сервис и HTTP logger
	if daemon.httpService, daemon.httpLogger, err = _http.New(daemon.ctx, &daemon.cfg.HttpServiceCfg, daemon.httpLogger); err != nil {
		return nil, err
	}

	// создаем обработчиков HTTP
	if daemon.httpHandler, err = httphandler.New(daemon.ctx, &daemon.cfg.HttpHandlerCfg, daemon.wpService, daemon.httpService); err != nil {
		return nil, err
	}

	// Установим HTTP обработчики
	if err = daemon.httpService.SetHttpHandler(daemon.ctx, daemon.httpHandler); err != nil {
		return nil, err
	}

	// Создаем HTTP server
	if daemon.httpServer, err = _httpserver.New(daemon.ctx, daemon.httpServerErrCh, &daemon.cfg.HttpServerCfg, daemon.httpService); err != nil {
		return nil, err
	}

	_log.Info("New daemon was created")

	return daemon, nil
}

// Run daemon and wait for system signal or error in error channel
func (d *Daemon) Run() error {
	_log.Info("Starting daemon")

	// запускаем сервис обработчиков - паники должны быть обработаны внутри
	go func() { d.wpServiceErrCh <- d.wpService.Run() }()

	// запускаем в фоне HTTP сервер, возврат в канал ошибок - паники должны быть обработаны внутри
	go func() { d.httpServerErrCh <- d.httpServer.Run() }()

	_log.Info("Daemon was running. For exit <CTRL-c>")

	// подписываемся на системные прикрывания
	signalCh := make(chan os.Signal, 1) // канал системных прибываний
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)

	// ожидаем прерывания или возврат в канал ошибок
	for {
		var err error
		select {
		case s := <-signalCh: //  ожидаем системное призывание
			_log.Info("Exiting, got signal", s)
			d.Shutdown(false, d.cfg.ShutdownTimeout) // останавливаем daemon
			return nil
		case err = <-d.httpServerErrCh: // возврат от HTTP сервера в канал ошибок
			_log.Info("Got error from HTTP")
		case err = <-d.wpServiceErrCh: // возврат от обработчиков в канал ошибок
			_log.Info("Got error from worker pool")
		}

		// от сервиса пришла пустая ошибка - игнорируем
		if err != nil {
			_log.Error(err.Error())                 // логируем ошибку
			d.Shutdown(true, d.cfg.ShutdownTimeout) // останавливаем daemon
			return err
		} else {
			_log.Info("Got empty error - ignore it")
		}
	}
}

// Shutdown daemon
func (d *Daemon) Shutdown(hardShutdown bool, shutdownTimeout time.Duration) {
	_log.Info("Shutting down daemon")

	// Закрываем корневой контекст
	defer d.cancel()

	//Останавливаем обработчик worker pool - прерываем обработку текущего задания
	if err := d.wpService.Shutdown(hardShutdown, shutdownTimeout); err != nil {
		_log.ErrorAsInfo(err) // дополнительно логируем результат остановки
	}

	// Останавливаем служебные сервисы
	if err := d.httpService.Shutdown(); err != nil {
		_log.ErrorAsInfo(err) // дополнительно логируем результат остановки
	}

	// Останавливаем HTTP сервер, ожидаем завершения активных подключений
	if err := d.httpServer.Shutdown(); err != nil {
		_log.ErrorAsInfo(err) // дополнительно логируем результат остановки
	}

	_log.Info("Daemon was shutdown")

	// Закрываем logger для корректного закрытия лог файла
	if err := d.httpLogger.Shutdown(); err != nil {
		_log.ErrorAsInfo(err) // дополнительно логируем результат остановки
	}
}
