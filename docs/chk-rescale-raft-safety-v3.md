# Безопасный rescale ClickHouseKeeperInstallation: hardened staged XML

## Статус документа

- Версия: 3.
- Статус: реализовано и прошло трёхстороннее whole-branch ревью
  (opus/codex/fable) в ветке `chk-rescale-raft-safety-design`; все шесть
  выявленных must-fix закрыты и отревьюены (см. «Итоги whole-branch ревью»).
  Floor полной поддержки rescale — Keeper 24.8 (см. «Поддержка версий Keeper»).
- База анализа: `Altinity/clickhouse-operator` `master`, commit
  `866e19f7820f663abc9708aa2a8d06183785cf98`; исходники ClickHouse Keeper
  23.8, 24.3, 25.8, master (проверка по коду 2026-07-20).
- Связанное предложение: gist Slach
  «how clickhouse-operator shall manage clickhouse-keeper scaling»
  (<https://gist.github.com/Slach/78d8105b3008b81f4c63f7aba305300e>).

Этот документ **заменяет v2**. Аудит исходников Keeper показал, что
центральный механизм v2 (versioned CAS у `reconfig`) не существует ни в одной
версии Keeper, а штатный XML-режим устроен надёжнее, чем предполагал v2.
Поэтому v3 возвращается к staged XML — подходу, предложенному Slach, — и
добавляет к нему барьеры подтверждения, закрывающие исходное split-brain окно.

Цели v3, зафиксированные заказчиком:

1. простое и понятное решение;
2. работа с Keeper начиная с 23.8 (требование — минимум с 24.8);
3. минимальное воздействие на код при максимальном результате;
4. упрощённая обработка сложных сценариев (fail-safe вместо полной
   state machine).

## Результаты аудита исходников

Все утверждения проверены по коду. Ссылки на Keeper даны по checkout
`upstream_25.8.2`, поведение идентично в 23.8 / 24.3 / 25.8 / master, если не
указано иное.

### Факт 1. Versioned CAS у `reconfig` не существует

`processReconfiguration` безусловно отвергает любой запрос с `version != -1`:

```cpp
if (!dispatcher.reconfigEnabled())
    return bad_request(ZUNIMPLEMENTED);
if (request.version != -1)
    return bad_request(ZBADVERSION);
```

(`src/Coordination/KeeperStateMachine.cpp:499-500`; в 23.8 — `:336-337`.)
Keeper никогда не сравнивает переданную версию с фактической. Версия
конфигурации клиентам не видна вообще: `get /keeper/config` возвращает только
список серверов, `Stat` не заполняется
(`src/Coordination/KeeperStorage.cpp:1784-1790`).

Вывод: дизайн v2 нереализуем как написан. Единственный CAS-подобный механизм —
JSON-команда `rcfg` с precondition по множеству member ID — существует только
в master и не подходит под требование «с 23.8».

### Факт 2. `reconfig` ненадёжнее XML-режима

Клиентский `reconfig` — оптимистичный и асинхронный: ответ `ZOK` означает
«поставлено в очередь», не «committed»; запрос не форвардится лидеру, и
отправленный follower'у может молча никогда не примениться
(`KeeperStateMachine.cpp:658-665`, master
`KeeperRequestDispatcher.cpp:782-786`).

XML-режим (`enable_reconfiguration=false`, поставляемый оператором по
умолчанию) устроен строже: при изменении конфиг-файла каждая нода вычисляет
diff между новым XML и **своим committed cluster config**
(`KeeperStateManager.cpp:537-605`), но применяет изменения **только лидер**,
**по одному серверу за раз**, с ретраями
(`KeeperDispatcher.cpp:502-553`, `KeeperServer.cpp:1335-1397`). NuRaft
дополнительно сериализует membership-изменения: следующее не принимается,
пока предыдущее не committed (`CONFIG_CHANGING`, `SERVER_IS_JOINING` в
`contrib/NuRaft/src/handle_join_leave.cxx:77-106`).

Вывод: правильный control plane для 23.8+ — staged XML, а не `reconfig`.
Миграция конфигурации не нужна: существующие кластеры уже в этом режиме.

### Факт 3. Split-brain окно реально, но требует двух свежих нод

Свежая нода с пустым хранилищем и XML `{0,1,2}` загружается как полноправный
voter всей тройки (`KeeperServer.cpp:501-504`: «No config in log store and
snapshot… Will use config from .xml on disk»). Две такие ноды могут избрать
лидера 2/3 между собой, пока сервер 0 продолжает работать с committed `{0}` и
кворумом 1/1 — сценарий v1/v2 подтверждён по коду выборов NuRaft
(`handle_vote.cxx:467-526`).

Однако **одна** свежая нода второй кворум образовать не может: для кворума ей
нужен голос существующего члена, а здоровый член с живым лидером pre-vote
отклоняет (`handle_vote.cxx:467-469`, условие `!hb_alive_`). Отсюда ключевая
инварианта дизайна: **в XML никогда не появляется больше одного сервера,
отсутствующего в committed membership**.

Дополнительный штатный guard — `<start_as_follower>true</start_as_follower>`:
нода не начинает выборы до первого heartbeat
(`KeeperStateManager.h:72-75`, NuRaft `raft_server.cxx:303-313`). Ограничения:
guard одноразовый (после первого heartbeat снимается и персистится), и NuRaft
запрещает ставить его на все серверы одновременно
(`KeeperStateManager.cpp:248-249`).

### Факт 4. `get /keeper/config` — пригодный барьер подтверждения

Возвращает сериализованный **committed** cluster config ноды в формате
`server.{id}={host:port};{participant|learner};{priority}`
(`KeeperReconfiguration.cpp:73-98`). Ответ локальный и может отставать, но
нода не может показать конфигурацию «из будущего» — только уже committed. Для
барьера «изменение зафиксировано» это безопасное направление ошибки: ложного
прохождения барьера быть не может, возможна только задержка.

### Факт 5. Поведение нод вне кворума

- Нода **без живого лидера** не принимает ни сессий, ни чтений, ни записей —
  всё отклоняется `ZCONNECTIONLOSS`/timeout
  (`KeeperDispatcher.cpp:271-320`, `KeeperTCPHandler.cpp:413`). Клиенты
  ClickHouse не могут читать stale-данные с изолированной ноды.
- Нода, **удалённая из committed config**, гасит Raft-активность, но процесс
  не завершается и продолжает отдавать stale-чтения подключённым клиентам,
  пока её не убьют снаружи (`handle_commit.cxx:1060-1084`; Keeper не
  обрабатывает callback `RemovedFromCluster` — `KeeperServer.cpp:1235-1236`).
  Удаление pod после committed removal обязательно.

### Факт 6. Наблюдаемость и версионные ограничения

- Роль ноды: `srvr`/`mntr` (`server_state`/`Mode`). На лидере `mntr` даёт
  `followers` и `synced_followers` (`FourLetterCommand.cpp:326-330`).
  Форматы ответов стабильны в 23.8→master: `srvr` — `Mode: <role>`, `mntr` —
  `zk_synced_followers\t<N>`, ответ `rqld` — `Sent leadership request to
  leader.` Важно: поставляемый оператором
  `config/chk/keeper_config.d/01-keeper-01-default-config.xml` задаёт
  `<four_letter_word_white_list>` БЕЗ `rqld` — в whitelist необходимо
  добавить `rqld` (в дефолтном allow-list самого Keeper он есть во всех
  версиях).
- 4LW-команды `mntr`/`srvr`/`stat`/`conf` отвечают только при живом лидере
  (`isServerActive`, `KeeperDispatcher.cpp:945-948`); `ruok` отвечает всегда.
- `conf` показывает **статический** конфиг, не committed membership.
  Committed membership доступен только через `get /keeper/config`.
- Перенос лидерства: `rqld` («передай лидерство мне») есть с 23.8;
  `ydld` и HTTP `/ready` появились в 24.3. Дизайн использует только `rqld`,
  поэтому floor = 23.8.

### Факт 7. Подтверждённое состояние оператора

- XML со **всеми** desired hosts публикуется сразу: исключение новых hosts
  задумано, но закомментировано (`pkg/controller/chk/worker.go:444-453`);
  селектор не исключает никого
  (`pkg/model/common/config/generator_options.go:98-114`).
- Механизм поштучного включения уже существует:
  `includeHostIntoRaftCluster` снимает атрибут `Exclude` и переписывает
  общий ConfigMap (`pkg/controller/chk/worker-wait-exclude-include-restart.go:97-108`).
- Вместо подтверждения membership — фиксированная задержка 7 секунд
  (`pkg/controller/chk/worker-reconciler-chk.go:828-842`).
- Cleanup при scale-down удаляет StatefulSet/PVC без какого-либо
  Raft-барьера (`pkg/controller/chk/worker-deleter.go:33-58`).
- PVC удаляемой реплики по умолчанию **удаляется**
  (`pkg/model/common/tags/labeler/auxiliary.go:51-52`: reclaim policy по
  умолчанию `Delete`) — повторное добавление реплики стартует с чистым диском.
- Leader election отсутствует и у CHK-, и у CHI-контроллера
  (`cmd/operator/app/thread_keeper.go:54-70`).
- В CHK-контроллере нет клиента Keeper; `pkg/model/zookeeper` умеет znode
  CRUD (включая `Get`) и используется только CHI-контроллером.
- `test_020005` проверяет только количество pod и репликацию через отдельный
  CHI; ни одного утверждения о Raft membership
  (`tests/e2e/test_operator.py:7292-7376`).

## Инварианты дизайна

1. XML `raft_configuration`, публикуемый оператором, содержит не более
   **одного** сервера, отсутствующего в committed membership (правило для
   add: именно свежие voters создают split-brain риск, см. Факт 3). Для
   remove допускается публикация сразу целевого множества: Keeper применяет
   удаления последовательно сам (очередь `KeeperDispatcher` +
   `CONFIG_CHANGING` в NuRaft, Факт 2), а безопасность обеспечивает
   инварианта 3.
2. Следующий add не публикуется, пока предыдущий не подтверждён барьером по
   `get /keeper/config`.
3. Kubernetes-ресурсы member удаляются только после подтверждённого
   отсутствия его `server_id` в committed membership.
4. Новые серверы, добавляемые к существующему кластеру, получают
   `start_as_follower=true`.
5. Если барьер не проходит (нет кворума, нет ответа, membership не совпал) —
   оператор останавливается, ставит Condition и повторяет попытку; он ничего
   не удаляет и не откатывает. Безопасно застрять лучше, чем угадать.
6. `CHK.status = Completed` выставляется только когда desired topology,
   committed membership (`/keeper/config`) и Kubernetes-ресурсы совпадают.

## Алгоритм

### Наблюдение (observe)

`ObserveMembership`: обойти известные member endpoints, выполнить
`get /keeper/config` (существующим клиентом `pkg/model/zookeeper`), распарсить
строки `server.N=host:port;participant;priority`. Дополнительно `srvr`/`mntr`
по TCP для роли и `synced_followers`.

Барьер `MembershipConverged(target)`: **все** члены target-множества отвечают
и возвращают ровно target-список. Недоступность любого члена — барьер не
пройден: requeue с Condition, без каких-либо действий. Это строже необходимого
(committed-факт наступает раньше), но однозначно и fail-safe.

### Scale-up: `{0} -> {0,1} -> {0,1,2}`

Для каждого добавляемого сервера, строго по одному:

1. Observe: committed membership совпадает с ожидаемым исходным множеством.
2. Опубликовать XML = committed + ровно один новый сервер; в конфиге нового
   сервера — `start_as_follower=true`.
3. Создать Service/StatefulSet нового сервера, дождаться старта процесса
   (лидер ретраит invite, пока pod не станет доступен — фиксированный sleep
   не нужен).
4. Барьер: `server.N` появился в `/keeper/config` на лидере и всех прежних
   членах.
5. Synced-барьер: на лидере `mntr` показывает
   `synced_followers >= voters - 1` (новый сервер догнал лог).
6. Перейти к следующему серверу.

Промежуточное чётное membership `{0,1}` требует обеих нод для кворума — это
документируемое availability-свойство, шаги не объединяются.

Первичный bootstrap нового кластера (все ноды новые, committed membership не
существует) остаётся как сейчас: полный XML сразу, штатные выборы NuRaft;
`start_as_follower` на всех нодах не ставится (NuRaft это запрещает).

### Scale-down: `{0,1,2} -> {0}`

1. Observe committed membership.
2. Если среди удаляемых серверов есть текущий лидер: `rqld` на выживающем
   сервере с наименьшим ordinal; ждать, пока лидером не станет выживающий
   сервер (`srvr`).
3. Опубликовать XML = desired (целевое множество). Удаления нескольких
   серверов Keeper-лидер применяет последовательно сам, каждое — через
   commit кворумом текущей конфигурации (Факт 2); публиковать их по одному
   не требуется.
4. Барьер: `/keeper/config` на всех выживающих членах равен ровно desired
   множеству.
5. Только после барьера удалить StatefulSet/Pod удалённых серверов (это же
   гасит процессы, способные отдавать stale-чтения).
6. PVC — по существующей reclaim policy (default `Delete`).

Если кворум отсутствует, шаг 3 никогда не подтвердится барьером — оператор
останавливается, не удаляя ни одного pod (инварианта 5).

### Изменение desired во время операции

Упрощённая политика вместо журнала операций v2:

- начатый шаг всегда доводится до барьера (или до таймаута с Condition);
- после барьера desired перечитывается и следующий шаг планируется от
  фактического committed membership;
- «противоположные» изменения (add затем remove того же сервера) — это просто
  два последовательных шага; специальной ABA-механики нет.

Повторное добавление реплики использует тот же `server_id` (= replica index).
Это безопасно **при чистом диске** и опасно при переиспользовании старого —
см. отдельный раздел «Известное ограничение: stale PVC при повторном
добавлении».

### Suspend, deletion, конфликты

- `spec.suspend` и `deletionTimestamp` запрещают начинать новый шаг; начатый
  шаг доводится до барьера.
- Если observe обнаруживает membership, не совпадающий ни с исходным, ни с
  целевым множеством текущего шага (ручной `reconfig`, внешнее вмешательство) —
  Condition `RaftMembershipConflict`, никаких mutation/delete до ручного
  разрешения.

## Известное ограничение: stale PVC при повторном добавлении реплики

**Статус: НЕ реализовано в этой ветке (отложено, см. ниже proposal).**

### Суть

`server_id` реплики равен её replica ordinal и **переиспользуется** при
повторном добавлении (`remove` реплики `N`, затем снова `add` реплики `N` — тот
же `server_id=N`). v3 сознательно НЕ выделяет новый Raft ID на каждую
инкарнацию (в отличие от v2), опираясь вместо этого на **чистоту диска**.

### Почему нужен чистый диск

Всё персистентное Raft-состояние NuRaft лежит на PVC реплики
(`/var/lib/clickhouse-keeper/coordination`): текущий `term`, лог, снапшоты,
последняя committed `cluster_config` и — критично — файл `srv_state`, где
хранится флаг разрешения election timer. Механизм безопасного добавления в v3
опирается на два свойства, которые верны **только на пустом диске**:

1. **NuRaft-семантика «нового joiner'а».** Когда лидер выполняет `add_srv`
   для `server_id`, которого нет в committed config, новая нода с пустым
   хранилищем становится `catching_up` learner'ом, получает лог/снапшот и
   становится voter'ом только после commit конфигурации, включающей её. Это
   безопасный путь.
2. **`start_as_follower` реально сдерживает выборы.** Флаг работает как
   «не начинать election до первого heartbeat». Но NuRaft **персистит** снятие
   этого запрета (`allow_election_timer`) на диск после первого же heartbeat.

Если PVC удаляемой реплики был сохранён (`reclaimPolicy: Retain`) и повторное
добавление примонтирует этот **старый диск**:

- новая инкарнация стартует не как чистый joiner, а с **устаревшими** `term`,
  логом и committed `cluster_config` (которая может ещё числить её voter'ом или
  содержать давно removed'ы);
- персистнутый `allow_election_timer=true` **обходит `start_as_follower`** —
  нода может немедленно начать выборы;
- это ровно то «member incarnation / PVC ABA» окно, которое v3 закрывает
  чистым диском: старый процесс/состояние неотличимы от новой попытки и могут
  образовать конфликтующий кворум → **split-brain или некорректный membership**.

### Почему при дефолте риск отсутствует

По умолчанию `reclaimPolicy: Delete`
(`pkg/model/common/tags/labeler/auxiliary.go:52`). На scale-down purge-барьер
(инвариант 3) удаляет PVC только **после** committed removal, а повторное
добавление провижнит **свежий пустой** PVC. Поэтому в штатной конфигурации
проблема не возникает by construction. Риск материализуется только при
явно выставленном `reclaimPolicy: Retain` **и** последовательности
remove→re-add того же ordinal.

### Обратный риск: `Delete` для всех = потеря состояния якоря

`Delete` безопасен для re-add, но у него есть **противоположная опасность**:
при scale-down `{0,1,2} -> {0}` кластер намеренно сводится к единственному
узлу — в этот момент node 0 является **единственной копией** всего
Keeper-состояния (метаданных всех реплицированных таблиц ClickHouse). Если её
PVC `Delete` и node 0 будет удалён (scale-to-0, удаление CHK, авария ноды) —
состояние теряется полностью.

Ключевое наблюдение: durability нужна **якорю**, а cleanliness — **транзитным**
нодам, и это **разные ноды**:

- **node 0 (и низкие ordinal'ы) переживают все rescale** — StatefulSet удаляет
  старшие ordinal'ы сверху (`{0,1,2} -> {0,1} -> {0}`), поэтому node 0 **никогда
  не попадает в цикл remove→re-add**. Значит stale-PVC ABA к ней **неприменим**,
  а `Retain` на ней безопасен и защищает состояние. Рестарт/перепланирование
  node 0 с её же диском — легитимный recovery (нода восстанавливает собственный
  committed `{0}`), а не ABA чужой инкарнации.
- **старшие ordinal'ы (1, 2) транзитны**: их removal committed, данные остаются
  на выживших, поэтому `Delete` их PVC **ничего не теряет**, а re-add получает
  чистый диск.

### Рекомендация для пользователей

Разделять reclaim policy по роли узла:

- **`Retain` на якоре** (низшие ordinal'ы, выживающие в минимальной topology —
  как минимум node 0) — durability без риска re-add ABA.
- **`Delete` на транзитных** старших ordinal'ах — чистый диск на re-add.

Если per-ordinal-политика недоступна и приходится выбирать одну на весь CHK:
предпочитайте `Retain` (потеря состояния необратима и катастрофична, а
stale-PVC ABA — узкий сценарий), и при `Retain` не переиспользуйте тот же
ordinal для новой реплики без ручной очистки его Raft data directory.

### Proposal по починке (направление, не спецификация)

Реализовать критерий готовности 6 — блокировать re-add на stale-диске, не
делая автоматической destructive-очистки:

0. **Differentiated reclaim policy (закрывает и потерю состояния, и ABA).**
   Поддержать per-ordinal reclaim policy: `Retain` на якоре (низшие
   выживающие ordinal'ы), `Delete` на транзитных старших. Так durability и
   cleanliness выполняются одновременно без ручного выбора. Требует, чтобы
   генератор/лейблер CHK умел назначать reclaim policy PVC по ordinal (сейчас
   она берётся из одного volumeClaimTemplate на весь CHK — нужна доработка).
   Порог «якорности» = минимальный `replicasCount`, до которого кластер может
   ужиматься (как минимум 1 → node 0).
1. **Пометить storage инкарнации.** При создании PVC писать в его annotations
   `chk-uid`, `member-id`, `incarnation-id` (ULID/uuid) и монотонный
   `storage-generation`. Incarnation ID хранить в `status.raft` рядом с
   `nextServerID`.
2. **Проверять перед стартом re-add.** Если для добавляемого host уже
   существует PVC (тот же детерминированный `data-<sts>-<ordinal>`), сверить
   его annotations с ожидаемой инкарнацией:
   - принадлежит другому `chk-uid` / другому `incarnation-id` / несёт непустое
     Raft-состояние прошлой инкарнации → **не запускать pod**, выставить
     Condition `RaftStorageIdentityValid=false` с указанием найденного PVC и
     инструкцией (сменить ordinal, очистить диск вручную, или разрешить
     политикой).
3. **Явная opt-in политика для авто-очистки.** Отдельное поле
   (`spec.keeperMembership.staleStorage: Block|Reuse|Wipe`, default `Block`) —
   `Wipe` разрешает оператору очистить Raft data directory перед стартом,
   `Reuse` (небезопасно) — принять старый диск. Никогда не очищать по умолчанию.
4. **Альтернатива (тяжелее, ближе к v2).** Выдавать новый, ранее не
   использованный `server_id` на каждую инкарнацию (`nextServerID` в status).
   Тогда старый процесс `memberID=N` не может выдать себя за новый; но это
   разводит replica ordinal и Raft identity и требует правок генератора и
   импорта существующих ID. v3 предпочитает более лёгкий вариант 1–3.

Тесты к реализации: unit на детект mismatch annotations → Condition; e2e
`1 -> 2 -> 1 -> 2` c `reclaimPolicy: Retain`, проверяющий, что второй `add`
блокируется, а не стартует на старом диске.

## Изменения кода

Все изменения точечные; новый пакет-«state machine» из v2 не нужен.

1. `pkg/controller/chk/worker.go` — `getRaftGeneratorOptions`: включить
   исключение hosts со статусом `Requested` из raft-конфига (доделать
   закомментированный селектор). XML начинает содержать только committed
   члены; `includeHostIntoRaftCluster` добавляет их по одному — механизм уже
   существует.
2. `pkg/model/chk/config/generator.go` — для hosts, добавляемых к
   существующему кластеру, генерировать `<start_as_follower>true</start_as_follower>`.
3. Новый небольшой хелпер (например, `pkg/model/chk/keeper/`):
   - `GetCommittedMembership(endpoints)` — `get /keeper/config` через
     существующий клиент `pkg/model/zookeeper` + парсер `server.N=...`;
   - `FourLetter(endpoint, cmd)` — одна TCP-запись/чтение для
     `srvr`/`mntr`/`rqld`;
   - учесть TLS/auth-настройки CHK при подключении.
4. `pkg/controller/chk/worker-reconciler-chk.go`:
   - заменить 7-секундный sleep (`:828-842`) на барьер
     «`server_id` появился в `/keeper/config`» + synced-барьер, с
     таймаутом и requeue;
   - не выставлять `Completed`, пока committed membership не совпал с desired.
5. Путь удаления host (`worker-deleter.go` + reconcile-цепочка): перед
   удалением StatefulSet удаляемого member — исключить его из XML, дождаться
   исчезновения из `/keeper/config`, при лидерстве — предварительно `rqld`
   на выживающем сервере. Generic `purge` не должен трогать StatefulSet
   member'а, чьё удаление ещё не подтверждено.
6. `cmd/operator/app/thread_keeper.go` — включить Lease leader election в
   options controller-runtime manager (стабильные имя/namespace Lease).
7. Conditions/Events (минимальный набор): `RaftMembershipConverged`,
   `RaftMembershipInProgress` (с указанием шага), `RaftMembershipStuck`
   (причина: нет кворума / таймаут барьера / конфликт membership / найден
   stale PVC).

## Поддержка версий Keeper

### Рекомендация

**Используйте последнюю стабильную версию ClickHouse Keeper (25.8+; для новых
установок — `latest`).** Rescale, split-brain safety и convergence-репортинг на
ней полностью работают и покрыты e2e-тестами (`test_020005*`).

### Официальный floor полной поддержки rescale: **24.8**

`24.8` и новее детерминированно проходят полный цикл `1 -> 3 -> 1`
(install → scale-up → scale-down, каждый шаг доходит до `Completed` с
подтверждённым committed membership). Проверено e2e на 24.8, 24.12, 25.3, 25.8.

Матрица механизмов, на которые опирается v3:

| Возможность | < 24.3 (вкл. 23.8) | 24.3–24.7 | 24.8+ | Роль в v3 |
|---|---|---|---|---|
| XML-diff membership (leader-driven, по одному) | да | да | да | основной механизм |
| `get /keeper/config` | да | да | да | барьер convergence |
| `mntr` `synced_followers` | да | да | да | synced-барьер |
| `rqld` | да | да | да | при удалении лидера |
| `start_as_follower` | да | да | да | split-brain guard |
| `async_replication` (coordination setting) | **нет** | да | да | gating (см. ниже) |
| `use_xid_64` (coordination setting) | **нет** | нет (до 24.12) | зависит | gating (см. ниже) |
| HTTP `http_control` / `/ready` (порт 9182) | **нет** | да | да | readiness pod'а |

### Что и почему НЕ работает на старых версиях

Оператор шлёт единый дефолтный Keeper-конфиг и единый набор Kubernetes-проб для
всех версий. Три вещи в них появились в Keeper позже и на старых версиях ломаются
— это **пред-существующие несовместимости оператора**, не связанные с raft-safety
(они одинаково ломают и `master`-оператор):

1. **`async_replication`** (coordination setting) — отсутствует до 24.3. Keeper
   отвергает неизвестную настройку с `UNKNOWN_SETTING` и **CrashLoop на старте**.
   *Смягчено в этой ветке:* оператор version-gate'ит эту настройку (эмитит только
   при Keeper ≥ 24.3), поэтому pod теперь стартует. См.
   `pkg/model/chk/config/generator.go` `getCoordinationSettings`.

2. **`use_xid_64`** (coordination setting) — отсутствует до 24.12. Тот же
   `UNKNOWN_SETTING`/CrashLoop; в частности **24.8 без gating не стартовал бы**.
   *Смягчено:* version-gate ≥ 24.12.

3. **HTTP readiness-проба `GET :9182/ready`** — использует `http_control`,
   которого нет до 24.3. На таких версиях порт 9182 не слушается, readiness-проба
   pod'а **всегда падает**, и pod никогда не переходит в `Ready`. Это **НЕ
   исправлено в этой ветке** и является причиной, по которой полный rescale на
   23.8 ненадёжен:

   > Так как pod никогда не `Ready`, `StatefulSet.Status.ReadyReplicas` остаётся 0,
   > и `k8s.IsStatefulSetReady()` (= `ReadyReplicas == Replicas`) всегда `false`.
   > В `doUpdateStatefulSet` (`pkg/controller/common/statefulset/statefulset-reconciler.go:269`)
   > успех update-пути гейтится этим условием, поэтому **любой StatefulSet-update
   > на такой версии трактуется как провал** → `ErrCRUDRecreate` (пересоздание STS)
   > → CHK не стабилизируется в `Completed`. На install это проявляется
   > интермиттентно (create-путь обычно доезжает), на scale-up — детерминированно
   > (update-путь неизбежен).

### Что на 23.8 всё же работает: safety

Важно разделять **безопасность** и **полный rescale-до-`Completed`**. Защита от
split-brain (staged XML по одному серверу + `start_as_follower`) — это свойство
**конфигурации**, оно не зависит ни от readiness-пробы, ни от чтения
`/keeper/config`. Поэтому e2e `test_020005_3` подтверждает: на нашем операторе
split-brain НЕ воспроизводится и на 23.8, и на 24.8, и на 25.8 (зелёный), тогда
как на старом небезопасном операторе он воспроизводится на всех трёх (красный с
двумя закоммиченными кворумами). То есть **на 23.8 кластер безопасен**, но
оператор не может надёжно довести его rescale до `Completed` из-за readiness-пробы.

### Итоговая политика

- **Полный rescale (create/scale-up/scale-down до `Completed`)**: floor **24.8**.
- **< 24.3 (вкл. 23.8)**: не поддерживается для управляемого rescale. Кластер
  безопасен (нет split-brain), но CHK может застревать в `InProgress`.
- **Чтобы полностью поддержать < 24.3** нужен третий version-gate того же класса,
  что coord-фикс: гейтить HTTP readiness-пробу (для Keeper без `http_control`
  отключать `:9182` или заменять на 4lw-based readiness, напр. `ruok`/`mntr`
  synced). Вынесено в отдельный тикет вместе с coordination-settings gating
  (`async_replication`/`use_xid_64`).

Не используемые v3 возможности (для полноты): `ydld` (есть с 24.3),
`reconfig` c versioned CAS (не существует ни в одной версии — см. Факт 1),
JSON `rcfg` c preconditions (только `master`).

## Итоги whole-branch ревью

Ветка прошла три независимых whole-branch ревью (opus / codex gpt-5.6-sol xhigh
/ fable xhigh). Happy-path (1→3→1, один shard, insecure, числовой тег Keeper)
надёжен и покрыт e2e; ревью нашли реальные дефекты на «стыках» и в смежных
конфигурациях. Все шесть must-fix закрыты и отревьюены:

1. **Version-gating на непарсящихся тегах** (3/3 ревьюера): `getHostSoftwareVersion`
   отдаёт `MinVersion()` (0.0.1, `IsKnown`), из-за чего coord-настройки молча
   выключались на свежих Keeper с тегом `head`/digest/кастом. Фикс: sentinel
   трактуется как unresolved → эмитим обе.
2. **Откат закоммиченных voter'ов** (codex/fable): прерванный reconcile посреди
   1→3 переопубликовывал `{0}` на ~30с и Keeper удалял committed 1,2. Фикс:
   стейджить только hosts без StatefulSet (+ перепроверка `IsNotFound`, не
   транзитная ошибка).
3. **Fresh-3-node deadlock** (codex): свежий `replicasCount:3` не доходил до
   `Completed` (bootstrap-классификация мутировала в цикле). Фикс: классификация
   join-vs-bootstrap по ancestor (`CRHasEstablishedCluster`), стабильно на весь
   reconcile.
4. **Secure-only (FIPS) CHK сломан** (fable/codex): гейты били в закрытый
   plaintext-порт. Реальный mTLS невозможен (оператор не провижнит client-cert;
   совпадает с существующим bail-out CHI ZK-интеграции). Фикс: для secure-only
   гейты деградируют к pre-branch поведению (не Aborted-петля) с Warning-событием;
   rescale-safety на secure-only НЕ верифицируется — см. follow-up.
5. **Stopped-CR livelock** (codex/fable): барьер и purge-гейт жгли 5 мин на
   остановленном CR. Фикс: `IsStopped` fast-path.
6. **metrics.CRReconcilesAborted** на convergence-gate failure (3/3): добавлен.

Риск CHI-реактивации ожившего `SetExclude`/`SetLowPriority` (Task 4) все три
ревью признали **benign** (exclude-тег путь в CHI — мёртвый код; оживает только
`low_priority`, host не выкидывается) — нужен лишь follow-up регрессионный тест.

## Тестовая стратегия

Упрощена относительно v2: без property-тестов state machine и полной
crash-матрицы; фокус на инвариантах 1–3, 6.

1. **Unit**: парсер `/keeper/config`; планировщик «не более одного сервера
   разницы между публикуемым XML и committed»; барьеры (fake-ответы);
   блокировка при stale PVC; отсутствие delete до подтверждения removal.
2. **E2E `test_020005` (обновлённый)**: `1 -> 3 -> 1`; после каждого шага —
   `get /keeper/config` совпадает с ожидаемым множеством на всех членах;
   ровно один лидер (`srvr`); непрерывная запись через ClickHouse во время
   rescale; `Completed` только после convergence.
3. **E2E split-brain guard**: имитация старого бага — проверка, что при
   scale-up 1→3 в XML любой ноды никогда не находятся два не-committed
   сервера одновременно (достаточно проверять содержимое ConfigMap и
   `/keeper/config` в цикле).
4. **E2E scale-down под отказом**: остановить один из трёх серверов, запросить
   3→1 — оператор останавливается с `RaftMembershipStuck`, ни один pod не
   удалён; после восстановления сервера операция доезжает.
5. **E2E ABA-lite**: `1 -> 2 -> 1 -> 2` подряд — конечная convergence, на
   каждом шаге инварианта 1 соблюдена.

## Сознательно принятые упрощения и residual risks

1. **Нет CAS-fence от внешнего писателя.** Ручной `reconfig`
   (при включённом кем-то `enable_reconfiguration`) или второй оператор,
   обошедший Lease, теоретически могут вмешаться между observe и публикацией
   XML. Смягчение: leader election + конфликт-детект на observe
   (инварианта 5). Принято как остаточный риск ради простоты.
2. **Нет журнала операций/эпох/инкарнаций.** Восстановление после рестарта
   оператора — обычный reconcile от фактического состояния: observe committed
   membership, сравнить с desired, продолжить. Возможные последствия — лишний
   повтор уже пройденного барьера, но не противоположная операция.
3. **`server_id` = replica index сохраняется.** Безопасность повторного
   добавления обеспечивается **чистым диском** (default PVC `Delete`), а не
   новыми ID. Блокировка re-add на сохранённом (`Retain`) stale-диске в этой
   ветке НЕ реализована — см. раздел «Известное ограничение: stale PVC при
   повторном добавлении» (там же proposal по починке).
4. **ConfigMap coalescing не опасен при инварианте 1**: kubelet может
   доставить только последнее состояние, но оператор публикует следующее
   состояние только после барьера, поэтому «пропущенных» промежуточных
   membership-шагов не существует по построению.
5. **Endpoint существующего member не меняется** (смена hostname при том же
   id аннулирует весь diff на стороне Keeper —
   `KeeperStateManager.cpp:564-573`). Оператор и так использует стабильные
   имена Service; фиксируем как ограничение.
6. **Floor полной поддержки — Keeper 24.8; < 24.3 не поддерживается для
   управляемого rescale.** Причина — пред-существующие несовместимости
   оператора с версиями до `http_control` (readiness `:9182`) и до
   coordination-настроек `async_replication`/`use_xid_64`. Подробно и с
   рекомендацией см. раздел «Поддержка версий Keeper». Полная поддержка < 24.3
   (version-gating readiness-пробы) вынесена в отдельный тикет.

## Отложено до отдельных задач

- Блокировка stale PVC при повторном добавлении (критерий готовности 6 ниже) —
  подробности, обоснование и proposal по починке в разделе «Известное
  ограничение: stale PVC при повторном добавлении».
- Формальные CRD Conditions из «Изменения кода» п.7 (пока Events + requeue).
- Version-gating HTTP readiness-пробы (`:9182`) для Keeper < 24.3 — вместе с
  уже сделанным coordination-settings gating это закрыло бы полную поддержку
  23.8; сейчас 23.8 safety-only.

Follow-up'ы из whole-branch ревью (реализация есть, но покрытие/усиление
отложено):

- **Real TLS observation для secure-only CHK** + secure-CHK e2e: сейчас гейты
  для secure-only деградируют (не верифицируются). Нужны client-cert
  provisioning в оператор и TLS-путь go-zk (см. Batch C).
- **E2E fresh-3-node** (свежий `replicasCount:3` до `Completed`) — гэп, который
  пропустил исходный e2e (делал только 1→3); фикс классификации есть, e2e —
  отложен.
- **CHI-регрессионный тест** на ожившую `low_priority`-machinery (Task 4).
- **Harden `SameIDs`**: сравнивать endpoint, а не только id (защита от
  same-id-at-stale-endpoint; см. residual risk 5).
- **Unit-тесты `worker-raft-membership.go`** (staging/`publishedMemberIDs`,
  `raftSafeToPurge` с/без STS/PVC).
- **Per-ordinal reclaim policy** (Retain-якорь / Delete-транзитные) — см.
  раздел про stale PVC.
- **Release-note по upgrade**: leader-election требует RBAC
  `coordination.k8s.io/leases` (образ-раньше-RBAC застопорит manager); перенос
  coord-настроек в `chop-generated-coordination.xml` + добавление `rqld` в
  whitelist меняют common ConfigMap → рестарты pod'ов существующих CHK.

## Критерии готовности

1. При `1 -> 3` ConfigMap ни в какой момент не содержит более одного
   не-committed сервера; каждый add подтверждён `/keeper/config` +
   `synced_followers` до следующего.
2. При `3 -> 1` StatefulSet удаляется только после исчезновения `server_id`
   из `/keeper/config`; при удалении лидера предварительно выполняется `rqld`.
3. Фиксированный 7-секундный sleep удалён.
4. При отсутствии кворума оператор блокируется с `RaftMembershipStuck`, не
   удаляя pod.
5. `Completed` означает desired == committed membership == ресурсы.
6. Повторное добавление реплики при retained stale PVC блокируется с
   Condition.
7. CHK-контроллер работает под Lease leader election.
8. Обновлённый `test_020005` и новые e2e (split-brain guard, scale-down под
   отказом, ABA-lite) зелёные; тест снят с flaky-списка.

## Отличия от v2 (для истории)

| v2 | v3 | Причина |
|---|---|---|
| `reconfig` + versioned CAS | staged XML + барьеры | CAS не существует (Факт 1); XML-режим надёжнее (Факт 2) |
| Журнал операций в status, эпохи | reconcile от фактического состояния | упрощение; observe даёт достаточно |
| Новые Raft ID на инкарнацию | `server_id` = replica index + блокировка stale PVC | PVC по умолчанию удаляется (Факт 7); минимизация кода |
| Миграция на `enable_reconfiguration=true` | не требуется | остаёмся в штатном режиме |
| Полная crash/property-матрица тестов | unit + 4 целевых e2e | упрощение при сохранении ключевых инвариант |

## Ограничение: secure-only Keeper (TLS) и деградация гейтов

Все новые Raft-гейты — барьер committed-membership (`get /keeper/config` по
ZK-протоколу), барьер `synced_followers` (4LW `mntr`), барьер purge и передача
лидерства (`rqld`) — обращаются к **плейнтекстовому** клиентскому порту Keeper
(`ZKPort`). Если у CR **все** хосты имеют закрытый плейнтекст-порт
(`!IsInsecure()` — тот же признак, по которому `getPlaintextListenerRemoval`
удаляет статический `<tcp_port>`), на `ZKPort` никто не слушает, и каждый гейт
падал бы на каждом reconcile → CR вечно `Aborted` / никогда `Completed` /
осиротевшие STS/PVC после scale-down.

Реальное TLS-наблюдение здесь невозможно без крупного нового плюминга: клиент
go-zookeeper поддерживает только mutual-TLS (требует клиентские `CertFile` +
`KeyFile`), а TLS Keeper-а в CHK задаётся целиком пользовательским серверным
`<openSSL>` XML, из которого оператор не может вывести клиентскую личность.
Клиентский сертификат оператору сегодня не выдаётся (ср.
`pkg/controller/chi/worker-zk-integration.go`, где по той же причине
пропускается TLS ZK-dial). Помимо этого, 4LW-команды (`srvr`/`mntr`/`rqld`)
Keeper вообще **не** обслуживает по `tcp_port_secure` (см. комментарий в
`pkg/model/chk/creator/probe.go`).

Поэтому для secure-only CR гейты **деградируют до до-branch-поведения**:
`crIsSecureOnlyKeeper` детектирует такой CR, и

- Completed-гейт помечает CR завершённым без проверки сходимости;
- барьеры join (`reconcileHostMainDomain`) пропускаются;
- purge-барьер (`raftSafeToPurge`) разрешает удаление без проверки.

Каждый пропуск громко фиксируется: Warning-событие
`RaftMembershipUnverified` + запись в `status.actions`. Гарантия безопасности
rescale для secure-only при этом **не** обеспечивается — это осознанный
компромисс, чтобы secure-CHK был не хуже, чем до этой ветки. Полноценное
TLS-наблюдение (клиентский сертификат оператору + ZK-чтение по `ZKPortSecure`)
— отдельная последующая задача; e2e-покрытие secure-CHK также follow-up.
