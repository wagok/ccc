# Project Instructions

## Task Tracking Policy

### ЕДИНСТВЕННЫЙ инструмент: beads (bd)

Для ЛЮБОГО отслеживания задач, прогресса, TODO используй ТОЛЬКО `bd`:

```bash
bd ready              # Поиск доступной работы
bd create "Task"      # Создание задачи
bd update <id> --status in_progress  # Начать работу
bd close <id>         # Завершить задачу
bd sync               # Синхронизация с git
```

### ЗАПРЕЩЕНО использовать для task tracking

| Инструмент | Статус | Причина |
|------------|--------|---------|
| **TodoWrite** | ЗАПРЕЩЁН | Используй `bd` вместо встроенного todo |
| **Linear MCP** | ЗАПРЕЩЁН | Внешняя система, не интегрирована |
| **Asana MCP** | ЗАПРЕЩЁН | Внешняя система, не интегрирована |
| **GitHub Issues** | Только upstream | Не для внутренних задач |
| **GitLab Issues** | ЗАПРЕЩЁН | Не используется |
| **Файлы TODO.md** | ЗАПРЕЩЁН | Задачи только в beads |

### Замена TodoWrite на beads

Когда нужно спланировать несколько задач, вместо TodoWrite:

```bash
# Создай задачи в beads
bd create "Implement feature X" -p 1
bd create "Add tests for X" -p 2
bd create "Update documentation" -p 3

# Отслеживай прогресс через bd
bd update <id> --status in_progress
bd close <id>
```

### Вспомогательные инструменты (read-only)

- **qdrant-mcp** — только поиск по документации
- **claude-context** — только анализ структуры кода
- **Grep/Glob** — поиск в коде

Эти инструменты НЕ используются для хранения задач или статусов.
