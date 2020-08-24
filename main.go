package main

import (
	"context"
	l "log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/carlescere/scheduler"
	"github.com/knadh/koanf"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/sirupsen/logrus"

	"github.com/NuclearLouse/logging"
)

// Global koanf instance. Use "." as the key path delimiter. This can be "/" or any character.
var (
	k   = koanf.New(".")
	log logrus.FieldLogger
	Jobs []*worker
)

type worker struct {
	Name        string
	Description string
	Script      string
	TimeRun     string
	Every       string
	Enabled     bool
	Status      string
	Prev        time.Time
	Next        time.Time
	Duration    string
}

func init() {
	var err error
	cfg := logging.NewConfig()
	cfg.LogFile = "shedule-scripts.log"
	log, err = logging.New(cfg)
	if err != nil {
		l.Fatalln("инициализация логера:", err)
	}
}

func main() {

	f := file.Provider("config.yml")
	workers, err := loadConfig(f)
	if err != nil {
		log.Fatalf("загрузка конфига: %v", err)
	}
	Jobs = workers
	schedulers := runAllJobs(workers)

	if err := f.Watch(func(event interface{}, err error) {
		if err != nil {
			log.Errorln("наблюдатель конфига:", err)
			return
		}

		log.Infoln("конфиг файл изменился. Перезагружаю конфиг ...")
		ss, err := restartService(f, schedulers)
		if err != nil {
			log.Fatalln("рестарт сервиса:", err)
		}
		schedulers = ss
	}); err != nil {
		log.Errorln("наблюдатель конфига:", err)
	}

	router := http.NewServeMux()
	router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		lp := filepath.Join("./", "statistic.html")
		tmpl, err := template.ParseFiles(lp)
		if err != nil {
			log.Error(err)
		}
		data := map[string]interface{}{}
		data["jobs"] = Jobs
		err = tmpl.ExecuteTemplate(w, "statistic.html", data)
		if err != nil {
			log.Error(err.Error())
			http.Error(w, http.StatusText(500), 500)
		}
	})
	srv := http.Server{
		Addr:    "localhost:7777",
		Handler: router,
	}

	idleConnsClosed := make(chan struct{})

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig)
		for {
			select {
			case c := <-sig:
				log.Infof("Получен сигнал: [%v]", c)
				switch c {
				case syscall.SIGHUP:

					ss, err := restartService(f, schedulers)
					if err != nil {
						log.Fatalln("рестарт сервиса:", err)
					}
					schedulers = ss

				case syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL, syscall.SIGABRT:
					// We received an interrupt signal, shut down.
					if err := srv.Shutdown(context.Background()); err != nil {
						// Error from closing listeners, or context timeout:
						log.Infof("HTTP server Shutdown: %v", err)
					}
					log.Infof("Сервер отключается...")
					close(idleConnsClosed)

				}
			default:
				time.Sleep(500 * time.Millisecond)
			}
		}

	}()
	log.Infoln("Сервер статистики старт ...", srv.Addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		// Error starting or closing listener:
		log.Fatalf("HTTP server ListenAndServe: %v", err)
	}

	<-idleConnsClosed

}

func restartService(f *file.File, ss []*scheduler.Job) ([]*scheduler.Job, error) {

	stopAllJobs(ss)
	workers, err := loadConfig(f)
	if err != nil {
		return nil, err
	}
	Jobs = workers
	return runAllJobs(workers), nil
}

func loadConfig(f *file.File) ([]*worker, error) {

	if err := k.Load(f, yaml.Parser()); err != nil {
		return nil, err
	}

	all := k.All()["workers"]
	cfg := all.([]interface{})
	var workers []*worker
	for _, section := range cfg {
		w := &worker{}
		for key, value := range section.(map[string]interface{}) {
			// fmt.Println(key, ":", value)
			if value != nil {
				switch key {
				case "name":
					w.Name = value.(string)
				case "description":
					w.Description = value.(string)
				case "script":
					w.Script = value.(string)
				case "enabled":
					w.Enabled = value.(bool)
				case "time_run":
					w.TimeRun = value.(string)
				case "every":
					w.Every = value.(string)
				}
			}

		}
		if w.Enabled {
			workers = append(workers, w)
		}

	}
	return workers, nil
}

func runAllJobs(ws []*worker) []*scheduler.Job {
	var scheds []*scheduler.Job
	var err error
	for _, w := range ws {
		s := parseEveryTime(w)
		s, err = func(s *scheduler.Job, f func()) (*scheduler.Job, error) {
			return s.Run(f)
		}(s, w.start)

		if err != nil {
			log.Errorf("запуск воркера [%s]:%s", w.Name, err)
			continue
		}
		scheds = append(scheds, s)
	}
	return scheds
}

func stopAllJobs(ss []*scheduler.Job) {
	for _, s := range ss {
		s.Quit <- true
	}
}

func (w *worker) start() {
	log.Infof("Воркер [%s] запуск скрипта:%s", w.Name, w.Script)
	w.Prev = time.Now()
	if err := func() error {
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "linux":
			cmd = exec.Command("bash", "-c", w.Script)
		case "windows":
			cmd = exec.Command("cmd", "/C", w.Script)
		}
		if err := cmd.Run(); err != nil {
			return err
		}
		return nil
	}(); err != nil {
		w.Status = "неудача"
		log.Errorf("Воркер [%s] команда не отработала: %s", w.Name, err)
		return
	}
	w.Duration = time.Since(w.Prev).String()
	w.Status = "успех"
	log.Infof("Воркер [%s] Команда успешно отработала. Длительность оперции: %v", w.Name, w.Duration)
}

func parseEveryTime(w *worker) *scheduler.Job {

	if w.TimeRun != "" {
		if containsAny(w.Every, "s", "m", "h") {
			return scheduler.Every().Day().At(w.TimeRun)
		}
		return everyScheduler(w.Every).At(w.TimeRun)
	}
	splEv := strings.Split(w.Every, " ")
	switch len(splEv) {
	case 1:
		return everyScheduler(w.Every)
	case 2:
		t, err := strconv.Atoi(splEv[0])
		if err != nil {
			t = 1
		}
		return everyScheduler(splEv[1], t)
	}
	return nil
}

func everyScheduler(every string, e ...int) *scheduler.Job {
	var t int
	if len(e) == 0 {
		t = 1
	} else {
		t = e[0]
	}
	// TODO: Из-за того, что я внес изменения в пакет - будет не правильно или вообще не будет работать
	// расписание по дням недели. Возможно стоит его вообще убрать.
	switch {
	case containsAny(every, "s"):
		return scheduler.Every(t).Seconds()
	case containsAny(every, "m"):
		return scheduler.Every(t).Minutes()
	case containsAny(every, "h"):
		return scheduler.Every(t).Hours()
	case containsAny(every, "mon", "пон"):
		return scheduler.Every(t).Monday()
	case containsAny(every, "tue", "вто"):
		return scheduler.Every(t).Tuesday()
	case containsAny(every, "wed", "сре"):
		return scheduler.Every(t).Wednesday()
	case containsAny(every, "thu", "чет"):
		return scheduler.Every(t).Thursday()
	case containsAny(every, "fri", "пят"):
		return scheduler.Every(t).Friday()
	case containsAny(every, "sat", "суб"):
		return scheduler.Every(t).Saturday()
	case containsAny(every, "sun", "вос"):
		return scheduler.Every(t).Sunday()
	}
	return scheduler.Every(t).Day()
}

func containsAny(s string, values ...string) bool {
	for _, v := range values {
		return strings.Contains(strings.ToLower(s), v)
	}
	return false
}
