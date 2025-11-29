package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Group struct {
	ID          int    `json:"id"`
	Description string `json:"description"`
	IsActive    string `json:"is_active"`
}

type Subnet struct {
	ID            int        `json:"id"`
	Subnet        string     `json:"subnet"`
	Description   string     `json:"description"`
	CreatedAt     *time.Time `json:"created_at,omitempty"`
	GroupID       int        `json:"group_id"`
	Gateway       *string    `json:"gateway,omitempty"`
	AlternativeIP *string    `json:"alternative_ip,omitempty"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
}

type PingResult struct {
	IP           string `json:"ip"`
	Alive        bool   `json:"alive"`
	Description  string `json:"description"`
	SourceSubnet string `json:"source_subnet"`
	Timestamp    string `json:"timestamp"`
	GroupID      int    `json:"group_id"`
	NetworkID    int    `json:"network_id"`
	Recurrences  int    `json:"recurrences"`
	FalseCount   int    `json:"false_count"`
}

type PreviousPingResults struct {
	Devices  []PingResult `json:"devices"`
	Datetime string       `json:"datetime"`
	Duration float64      `json:"duration,omitempty"` // Длительность в миллисекундах
}

type PingState struct {
	Alive       bool
	GroupID     int
	NetworkID   int
	Recurrences int  // Оставляем для обратной совместимости
	FalseCount 	int  // Новый счетчик только для false-состояний
}

func readLastPingFile() (map[string]PingState, error) {
	// Проверить, существует ли файл
	if _, err := os.Stat("last_ping.json"); os.IsNotExist(err) {
		// Файл не существует, создаем его с пустой структурой
		initialData := PreviousPingResults{
			Devices:  []PingResult{},
			Datetime: "",
		}
		data, err := json.MarshalIndent(initialData, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile("last_ping.json", data, 0644); err != nil {
			return nil, err
		}
		// Файл создан, возвращаем пустую карту
		return make(map[string]PingState), nil
	}

	data, err := os.ReadFile("last_ping.json")
	if err != nil {
		return nil, err
	}

	var results PreviousPingResults
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, err
	}

	// Преобразовать в карту для более удобного поиска
	pingStates := make(map[string]PingState)
	for _, device := range results.Devices {
		// Используем уникальный ключ для IP-подсеть, чтобы различать одинаковые IP в разных подсетях
		key := fmt.Sprintf("%s_%d", device.IP, device.NetworkID)
		pingStates[key] = PingState{
			Alive:       device.Alive,
			GroupID:     device.GroupID,
			NetworkID:   device.NetworkID,
			Recurrences: device.Recurrences,
			FalseCount:  device.FalseCount, // Используем значение из файла
		}
	}

	return pingStates, nil
}

// Функция для определения подсети, к которой принадлежит IP
func findSubnetForIP(ipStr string, subnets []Subnet) *Subnet {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return nil
	}

	for _, subnet := range subnets {
		// Проверка основной подсети
		_, ipNet, err := net.ParseCIDR(subnet.Subnet)
		if err == nil && ipNet.Contains(ip) {
			return &subnet
		}

		// Проверка альтернативного IP
		if subnet.AlternativeIP != nil && *subnet.AlternativeIP == ipStr {
			return &subnet
		}
	}

	return nil
}

// Функция для записи изменения состояния в ping_history
func insertPingHistory(ctx context.Context, db *pgxpool.Pool, groupID, networkID int, ip string, state bool) error {
	query := `INSERT INTO ping_history (group_id, network_id, ipaddress, state, changed_at) VALUES ($1, $2, $3, $4, NOW())`
	_, err := db.Exec(ctx, query, groupID, networkID, ip, state)
	return err
}

func createPingHistoryTable(ctx context.Context, db *pgxpool.Pool) error {
	query := `
	CREATE TABLE IF NOT EXISTS ping_history (
		id SERIAL PRIMARY KEY,
		group_id INT NOT NULL,
		network_id INT NOT NULL,
		ipaddress INET NOT NULL,
		state BOOLEAN NOT NULL,
		changed_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
	);
	`
	_, err := db.Exec(ctx, query)
	return err
}

func incIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

func fetchGroupsFromDB(ctx context.Context, db *pgxpool.Pool) ([]int, error) {
	rows, err := db.Query(ctx, `
        SELECT id 
        FROM groups 
        WHERE is_active = true
    `)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []int

	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		groups = append(groups, id)
	}

	return groups, rows.Err()
}

func fetchSubnetsFromDB(ctx context.Context, db *pgxpool.Pool, groupIDs []int) ([]Subnet, error) {
	// Если нет активных групп — возвращаем пустой список
	if len(groupIDs) == 0 {
		return []Subnet{}, nil
	}

	// Генерируем параметризованный IN (...)
	// Например: $1, $2, $3
	params := make([]string, len(groupIDs))
	args := make([]interface{}, len(groupIDs))
	for i, id := range groupIDs {
		params[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}

	query := fmt.Sprintf(`
        SELECT
            id,
            network_address,
            description,
            created_at,
            group_id,
            gateway_ip,
            alternative_ip,
            updated_at
        FROM networks
        WHERE is_active = true
          AND group_id IN (%s)
    `, strings.Join(params, ", "))

	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subnets []Subnet

	for rows.Next() {
		var (
			s             Subnet
			createdAt     pgtype.Timestamp
			updatedAt     pgtype.Timestamp
			gateway       pgtype.Text
			alternativeIP pgtype.Text
		)

		err := rows.Scan(
			&s.ID,
			&s.Subnet,
			&s.Description,
			&createdAt,
			&s.GroupID,
			&gateway,
			&alternativeIP,
			&updatedAt,
		)
		if err != nil {
			return nil, err
		}

		if createdAt.Valid {
			t := createdAt.Time
			s.CreatedAt = &t
		}

		if updatedAt.Valid {
			t := updatedAt.Time
			s.UpdatedAt = &t
		}

		if gateway.Valid {
			s.Gateway = &gateway.String
		}

		if alternativeIP.Valid {
			s.AlternativeIP = &alternativeIP.String
		}

		subnets = append(subnets, s)
	}

	return subnets, rows.Err()
}

// Опрос уникальных альтернативных IP-адресов, результаты уходят в resultsCh
func pollAlternativeIPs(ctx context.Context, uniqueAltIPs []string, workers int, fpingTimeoutMs int, resultsCh chan<- map[string]bool) {
	if workers <= 0 {
		workers = 10
	}

	if len(uniqueAltIPs) == 0 {
		// Если нет альтернативных IP, отправляем пустой результат и возвращаемся
		resultsCh <- make(map[string]bool)
		return
	}

	var wg sync.WaitGroup
	jobs := make(chan []string)

	// Разбиваем список альтернативных IP на чанки для обработки
	chunkSize := 200 // Обрабатываем по 200 IP за раз, чтобы не перегружать fping
	var chunks [][]string
	for i := 0; i < len(uniqueAltIPs); i += chunkSize {
		end := i + chunkSize
		if end > len(uniqueAltIPs) {
			end = len(uniqueAltIPs)
		}
		chunks = append(chunks, uniqueAltIPs[i:end])
	}

	// Запуск воркеров
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunk := range jobs {
				select {
				case <-ctx.Done():
					return
				default:
				}

				// Используем fping для опроса списка IP-адресов
				args := []string{"-c", "2", "-t", fmt.Sprintf("%d", fpingTimeoutMs), "-a"}
				args = append(args, chunk...) // Добавляем все IP-адреса в команду
				cmd := exec.CommandContext(ctx, "fping", args...)

				output, err := cmd.CombinedOutput()

				// fmt.Printf("\n[fping alternative IPs] cmd: fping %s\n", strings.Join(args, " "))

				if err != nil {
					if exitErr, ok := err.(*exec.ExitError); ok {
						exitCode := exitErr.ExitCode()
						// код 1 у fping — недоступные хосты, не считаем ошибкой
						if exitCode != 1 {
							log.Printf("Ошибка fping для альтернативных IP, код %d\n", exitCode)
						}
					} else {
						log.Printf("Не удалось выполнить fping для альтернативных IP: %v\n", err)
					}
				}

				// Разбираем результаты
				ipResults := make(map[string]bool)

				// Сначала помечаем все IP как мертвые
				for _, ip := range chunk {
					ipResults[ip] = false
				}

				// Затем помечаем живые IP, если в строке есть "min/avg/max"
				for _, line := range strings.Split(string(output), "\n") {
					line = strings.TrimSpace(line)
					if line == "" {
						continue
					}

					// Проверяем, содержит ли строка "min/avg/max" - это признак живого устройства
					if strings.Contains(line, "min/avg/max") {
						parts := strings.Split(line, " ")
						if len(parts) > 0 {
							ipPart := strings.Split(parts[0], ":")[0] // Извлекаем IP из "IP_ADDRESS:"
							if ipPart != "" {
								// Проверяем, находится ли IP в списке опрашиваемых
								for _, targetIP := range chunk {
									if targetIP == ipPart {
										ipResults[ipPart] = true
										break
									}
								}
							}
						}
					}
				}

				resultsCh <- ipResults
			}
		}()
	}

	// Отправка задач воркерам
	for _, chunk := range chunks {
		jobs <- chunk
	}
	close(jobs)

	// Ждём завершения всех воркеров
	wg.Wait()
}

// Опрос подсетей, результаты уходят в resultsCh
func pollSubnets(ctx context.Context, subnets []Subnet, workers int, fpingTimeoutMs int, resultsCh chan<- map[string]bool) {
	if workers <= 0 {
		workers = 10
	}

	var wg sync.WaitGroup
	jobs := make(chan Subnet)

	// Запуск воркеров
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range jobs {
				select {
				case <-ctx.Done():
					return
				default:
				}

				// Используем fping с флагом -g для опроса подсети
				args := []string{"-c", "2", "-t", fmt.Sprintf("%d", fpingTimeoutMs), "-a", "-g", s.Subnet}
				cmd := exec.CommandContext(ctx, "fping", args...)

				output, err := cmd.CombinedOutput()

				// fmt.Printf("\n[fping subnet %s] cmd: fping %s\n", s.Subnet, strings.Join(args, " "))

				if err != nil {
					if exitErr, ok := err.(*exec.ExitError); ok {
						exitCode := exitErr.ExitCode()
						// Код 1 у fping — недоступные хосты, не считаем ошибкой
						if exitCode != 1 {
							log.Printf("Ошибка fping для подсети %s, код %d\n", s.Subnet, exitCode)
						}
					} else {
						log.Printf("Не удалось выполнить fping для подсети %s: %v\n", s.Subnet, err)
					}
				}

				// Разбираем результаты
				ipResults := make(map[string]bool)

				// Сначала определяем опрашиваемые IP-адреса из подсети
				ips, err := generateIPs(s.Subnet)
				if err != nil {
					log.Printf("Ошибка генерации IP для подсети %s: %v", s.Subnet, err)
					continue
				}

				// Исключаем шлюз, если он есть
				if s.Gateway != nil && *s.Gateway != "" {
					filteredIPs := make([]string, 0)
					for _, ip := range ips {
						if ip != *s.Gateway {
							filteredIPs = append(filteredIPs, ip)
						}
					}
					ips = filteredIPs
				}

				// Сначала помечаем все IP как мертвые
				for _, ip := range ips {
					ipResults[ip] = false
				}

				// Затем помечаем живые IP, если в строке есть "min/avg/max"
				for _, line := range strings.Split(string(output), "\n") {
					line = strings.TrimSpace(line)
					if line == "" {
						continue
					}

					// Проверяем, содержит ли строка "min/avg/max" - это признак живого устройства
					if strings.Contains(line, "min/avg/max") {
						parts := strings.Split(line, " ")
						if len(parts) > 0 {
							ipPart := strings.Split(parts[0], ":")[0] // Извлекаем IP из "IP_ADDRESS:"
							if ipPart != "" {
								// Проверяем, находится ли IP в списке опрашиваемых
								for _, targetIP := range ips {
									if targetIP == ipPart {
										ipResults[ipPart] = true
										break
									}
								}
							}
						}
					}
				}

				resultsCh <- ipResults
			}
		}()
	}

	// Отправка задач воркерам
	for _, s := range subnets {
		jobs <- s
	}
	close(jobs)

	// Ждём завершения всех воркеров
	wg.Wait()
}


// Генерация IP-адресов из CIDR
func generateIPs(cidr string) ([]string, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}

	var ips []string
	for ip := ipNet.IP.Mask(ipNet.Mask); ipNet.Contains(ip); incIP(ip) {
		ips = append(ips, ip.String())
	}
	return ips, nil
}

// Обновлённый `runPinger`
func runPinger(ctx context.Context, db *pgxpool.Pool, cfg Config) {
	// Start measuring execution time
	startTime := time.Now()

	// Получение ID активных групп
	groupIDs, err := fetchGroupsFromDB(ctx, db)
	if err != nil {
		log.Println("Ошибка получения групп из базы данных:", err)
		return
	}

	// Получение подсетей по активным группам
	subnets, err := fetchSubnetsFromDB(ctx, db, groupIDs)
	if err != nil {
		log.Println("Ошибка получения подсетей из базы данных:", err)
		return
	}

	if len(subnets) == 0 {
		log.Println("Подсетей для опроса не найдено")
		return
	}

	log.Printf("Получено подсетей: %d\n", len(subnets))

	// Загрузка предыдущих результатов
	pingStates, err := readLastPingFile()
	if err != nil {
		log.Printf("Ошибка чтения предыдущих результатов из файла: %v", err)
		// Если не удалось прочитать файл, инициализируем пустую мапу
		pingStates = make(map[string]PingState)
	}

	// Каналы для результатов опроса
	subnetResultsCh := make(chan map[string]bool)
	altIPResultsCh := make(chan map[string]bool)

	// Собираем уникальные альтернативные IP-адреса
	uniqueAltIPs := make(map[string][]*Subnet) // IP -> список подсетей, которым он принадлежит
	uniqueAltIPStrings := make([]string, 0)    // Плоский список уникальных IP для передачи в fping
	seenIPs := make(map[string]bool)          // Для отслеживания уникальности IP

	for i := range subnets {
		if subnets[i].AlternativeIP != nil && *subnets[i].AlternativeIP != "" {
			ip := *subnets[i].AlternativeIP
			if !seenIPs[ip] {
				uniqueAltIPStrings = append(uniqueAltIPStrings, ip)
				seenIPs[ip] = true
			}
			uniqueAltIPs[ip] = append(uniqueAltIPs[ip], &subnets[i])
		}
	}

	// Структура для хранения результатов опроса
	var allResults []PingResult

	// Для избежания дубликации результатов - отслеживаем уже обработанные IP
	processedResults := make(map[string]bool)

	// Горутина для обработки результатов
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()

		// Обработка результатов опроса подсетей
		for ipResults := range subnetResultsCh {
			for ip, alive := range ipResults {
				// fmt.Printf("%s >>> %t\n", ip, alive)

				// Используем уникальный ключ для избежания дублирования
				// Важно понимать, что один IP может быть альтернативным для нескольких подсетей,
				// но альтернативные IP обрабатываются отдельно, чтобы избежать дублирования
				subnet := findSubnetForIP(ip, subnets)
				if subnet != nil {
					// Пропускаем альтернативные IP, они обрабатываются отдельно в альтернативных IP результатах
					if subnet.AlternativeIP != nil && *subnet.AlternativeIP == ip {
						continue // Пропускаем, так как альтернативные IP обрабатываются в другом месте
					}

					// Обычный IP из подсети
					// Для избежания дублирования, используем уникальный ключ IP+NetworkID
					key := fmt.Sprintf("%s_%d", ip, subnet.ID)
					// Проверяем, не обработали ли мы уже этот IP для этой подсети
					mapKey := fmt.Sprintf("ip_%s_subnet_%d", ip, subnet.ID)
					if processedResults[mapKey] {
						continue // Уже обработан, пропускаем
					}
					processedResults[mapKey] = true

					// Проверяем изменение состояния
					oldState, exists := pingStates[key]
					newState := PingState{
						Alive:      alive,
						GroupID:    subnet.GroupID,
						NetworkID:  subnet.ID,
						Recurrences: 1,
						FalseCount:  0,
					}

					if exists {
						// Сценарий: true -> true
						if oldState.Alive && alive {
							newState.Recurrences = oldState.Recurrences + 1
							newState.FalseCount = 0
						} else if oldState.Alive && !alive {
							// Сценарий: true -> false (1st occurrence)
							newState.Recurrences = oldState.Recurrences + 1
							newState.FalseCount = oldState.FalseCount + 1
							// Оставляем alive = true до тех пор, пока не будет 3 подтверждений
							newState.Alive = true
						} else if !oldState.Alive && alive {
							// Сценарий: false -> true
							newState.Recurrences = 1
							newState.FalseCount = 0
						} else if !oldState.Alive && !alive {
							// Сценарий: false -> false
							newState.Recurrences = oldState.Recurrences + 1
							newState.FalseCount = 0 // FalseCount используется только при переходе true->false
						}
					} else {
						// Сценарий: Новое устройство
						if alive {
							// Новое true - recurrences=1, FalseCount=0, alive=True
							newState.Recurrences = 1
							newState.FalseCount = 0
						} else {
							// Новое false - recurrences=1, FalseCount=1, alive=False
							newState.Recurrences = 1
							newState.FalseCount = 1
						}
					}

					// Записываем в БД по новым правилам в соответствии с README таблицей
					writeToDB := false

					// Сценарии записи в БД:
					// true -> false (3rd occurrence) - Да (FalseCount >= false_count_threshold)
					// false -> true - Да
					if !oldState.Alive && alive && exists {
						// false -> true: записываем в БД
						writeToDB = true
					} else if oldState.Alive && !alive && exists && newState.FalseCount >= cfg.FalseCountThreshold {
						// true -> false (threshold occurrence): записываем в БД
						writeToDB = true
						newState.Recurrences = cfg.FalseCountThreshold  // Фиксируем количество повторений из конфига
					} else if !exists && alive {
						// Новое true: не записываем в БД
						writeToDB = false
					} else if !exists && !alive {
						// Новое false: не записываем в БД (FalseCount=1)
						writeToDB = false
					}

					// После проверки условий, обновляем newState.Alive, если FalseCount достиг порога
					if oldState.Alive && !alive && exists && newState.FalseCount >= cfg.FalseCountThreshold {
						newState.Alive = false // alive становится false после порога
						newState.FalseCount = 0 // Обнуляем, т.к. alive теперь false
					}

					if writeToDB {
						if err := insertPingHistory(ctx, db, newState.GroupID, newState.NetworkID, ip, newState.Alive); err != nil {
							log.Printf("Ошибка вставки в ping_history: %v", err)
						}
					}

					pingStates[key] = newState

					// Добавляем в результаты
					allResults = append(allResults, PingResult{
						IP:           ip,
						Alive:        newState.Alive,
						Description:  subnet.Description,
						SourceSubnet: subnet.Subnet,
						Timestamp:    time.Now().Format("2006-01-02 15:04:05"),
						GroupID:      subnet.GroupID,
						NetworkID:    subnet.ID,
						Recurrences:  newState.Recurrences,
						FalseCount:   newState.FalseCount,
					})
				}
			}
		}
	}()

	// Горутина для обработки результатов опроса альтернативных IP
	wg.Add(1)
	go func() {
		defer wg.Done()

		// Обработка результатов опроса альтернативных IP
		for ipResults := range altIPResultsCh {
			for ip, alive := range ipResults {

				// Альтернативный IP может принадлежать нескольким подсетям
				if altSubnets, exists := uniqueAltIPs[ip]; exists {
					for _, altSubnet := range altSubnets {
						// Проверяем, изменилось ли состояние
						key := fmt.Sprintf("%s_%d", ip, altSubnet.ID) // Уникальный ключ для IP-подсеть
						oldState, exists := pingStates[key]
						newState := PingState{
							Alive:     alive,
							GroupID:   altSubnet.GroupID,
							NetworkID: altSubnet.ID,
							// Увеличиваем Recurrences, если IP был в том же состоянии
							Recurrences: 1,
						}

						if exists {
							// Сценарий: true -> true
							if oldState.Alive && alive {
								newState.Recurrences = oldState.Recurrences + 1
								newState.FalseCount = 0
							} else if oldState.Alive && !alive {
								// Сценарий: true -> false (1st occurrence)
								newState.Recurrences = oldState.Recurrences + 1
								newState.FalseCount = oldState.FalseCount + 1
								// Оставляем alive = true до тех пор, пока не будет 3 подтверждений
								newState.Alive = true
							} else if !oldState.Alive && alive {
								// Сценарий: false -> true
								newState.Recurrences = 1
								newState.FalseCount = 0
							} else if !oldState.Alive && !alive {
								// Сценарий: false -> false
								newState.Recurrences = oldState.Recurrences + 1
								newState.FalseCount = 0 // FalseCount используется только при переходе true->false
							}
						} else {
							// Сценарий: Новое устройство
							if alive {
								// Новое true - recurrences=1, FalseCount=0, alive=True
								newState.Recurrences = 1
								newState.FalseCount = 0
							} else {
								// Новое false - recurrences=1, FalseCount=1, alive=False
								newState.Recurrences = 1
								newState.FalseCount = 1
							}
						}

						// Применяем ту же логику для альтернативных IP
						writeToDB := false

						// Сценарии записи в БД:
						// true -> false (threshold occurrence) - Да (FalseCount >= false_count_threshold)
						// false -> true - Да
						if !oldState.Alive && alive && exists {
							// false -> true: записываем в БД
							writeToDB = true
						} else if oldState.Alive && !alive && exists && newState.FalseCount >= cfg.FalseCountThreshold {
							// true -> false (threshold occurrence): записываем в БД
							writeToDB = true
							newState.Recurrences = cfg.FalseCountThreshold  // Фиксируем количество повторений из конфига
						} else if !exists && alive {
							// Новое true: не записываем в БД
							writeToDB = false
						} else if !exists && !alive {
							// Новое false: не записываем в БД (FalseCount=1)
							writeToDB = false
						}

						// После проверки условий, обновляем newState.Alive, если FalseCount достиг порога
						if oldState.Alive && !alive && exists && newState.FalseCount >= cfg.FalseCountThreshold {
							newState.Alive = false // alive становится false после порога
							newState.FalseCount = 0 // Обнуляем, т.к. alive теперь false
						}

						if writeToDB {
							if err := insertPingHistory(ctx, db, newState.GroupID, newState.NetworkID, ip, newState.Alive); err != nil {
								log.Printf("Ошибка вставки в ping_history для альтернативного IP: %v", err)
							}
						}

						pingStates[key] = newState

						// Добавляем в результаты
						allResults = append(allResults, PingResult{
							IP:           ip,
							Alive:        newState.Alive,
							Description:  altSubnet.Description,
							SourceSubnet: altSubnet.Subnet,
							Timestamp:    time.Now().Format("2006-01-02 15:04:05"),
							GroupID:      altSubnet.GroupID,
							NetworkID:    altSubnet.ID,
							Recurrences:  newState.Recurrences,
							FalseCount:   newState.FalseCount,
						})
					}
				}
			}
		}
	}()

	// Одновременный опрос подсетей и альтернативных IP
	go func() {
		pollSubnets(ctx, subnets, cfg.Workers, cfg.FpingTimeoutMs, subnetResultsCh)
		close(subnetResultsCh)
	}()

	go func() {
		pollAlternativeIPs(ctx, uniqueAltIPStrings, cfg.Workers, cfg.FpingTimeoutMs, altIPResultsCh)
		close(altIPResultsCh)
	}()

	// Ждем завершения обработки всех результатов
	wg.Wait()

	// Вычисляем продолжительность выполнения
	duration := time.Since(startTime).Seconds()

	// Сохраняем результаты в JSON файл в соответствии с форматом из README
	resultJSON := PreviousPingResults{
		Devices:  allResults,
		Datetime: time.Now().Format("2006-01-02 15:04:05"),
		Duration: duration,
	}

	// Сохраняем в файл
	data, err := json.MarshalIndent(resultJSON, "", "  ")
	if err != nil {
		log.Printf("Ошибка маршалинга результатов в JSON: %v", err)
	} else {
		if err := os.WriteFile("last_ping.json", data, 0644); err != nil {
			log.Printf("Ошибка записи результатов в файл: %v", err)
		}
	}
}

type Config struct {
	DatabaseURL         string `json:"database_url"`
	PollIntervalSec     int    `json:"poll_interval_seconds"`
	Workers             int    `json:"workers"`
	FalseCountThreshold int    `json:"false_count_threshold"`
	FpingTimeoutMs      int    `json:"fping_timeout_ms"`
}

func loadDBConfig(path string) (Config, error) {
	// Простая загрузка конфигурации из JSON-файла
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()

	var cfg Config
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return Config{}, err
	}

	// Значение по умолчанию, если не указано в конфиге
	if cfg.PollIntervalSec <= 0 {
		cfg.PollIntervalSec = 10
	}

	// Установка значения по умолчанию для FalseCountThreshold, если не указано
	if cfg.FalseCountThreshold <= 0 {
		cfg.FalseCountThreshold = 3
	}

	return cfg, nil
}

func main() {
	// Основной контекст приложения с возможностью отмены
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Обработка сигналов завершения (Ctrl+C, SIGTERM)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Получен сигнал завершения, останавливаемся...")
		cancel()
	}()

	// Загрузка конфигурации из конфиг-файла
	cfg, err := loadDBConfig("dbconfig.json")
	if err != nil {
		log.Fatal("Ошибка загрузки конфигурации БД:", err)
	}

	// Проверка, есть ли переменная окружения DATABASE_URL и использовать её, если есть
	if envDBURL := os.Getenv("DATABASE_URL"); envDBURL != "" {
		cfg.DatabaseURL = envDBURL
		log.Println("Используем DATABASE_URL из переменной окружения")
	}

	db, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal("Ошибка подключения к PostgreSQL:", err)
	}
	defer db.Close()

	// Создание таблицы ping_history, если она не существует
	if err := createPingHistoryTable(ctx, db); err != nil {
		log.Fatal("Ошибка создания таблицы ping_history:", err)
	}

	// Выполнить runPinger сразу при запуске
	fmt.Println("Скрипт запущен (первый запуск)")
	runPinger(ctx, db, cfg)
	fmt.Println("Скрипт остановлен (первый запуск)")

	interval := time.Duration(cfg.PollIntervalSec) * time.Second
	log.Printf("Интервал опроса: %v\n", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Контекст отменён, выходим из цикла main")
			return
		case <-ticker.C:
			fmt.Println("Скрипт запущен")
			runPinger(ctx, db, cfg)
			fmt.Println("Скрипт остановлен")
		}
	}
}
