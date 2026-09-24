"""Small PostgreSQL lexical boundary scanner, not a SQL semantic validator.

Comments and literal contents never become keywords. Quoted identifiers become
opaque identifiers. Unterminated input fails closed. WHERE is checked at the
same parenthesis depth as its UPDATE/DELETE, not inside a subquery.
"""
from __future__ import annotations

import re

_DOLLAR = re.compile(r"\$(?:[A-Za-z_][A-Za-z_0-9]*)?\$")
_WORD = re.compile(r"[A-Za-z_][A-Za-z_0-9$]*")


def tokens(sql: str) -> list[str]:
    result: list[str] = []
    i = 0
    while i < len(sql):
        c = sql[i]
        if c.isspace():
            i += 1
        elif sql.startswith('--', i):
            end = sql.find('\n', i + 2)
            i = len(sql) if end < 0 else end + 1
        elif sql.startswith('/*', i):
            depth = 1
            i += 2
            while i < len(sql) and depth:
                if sql.startswith('/*', i):
                    depth += 1
                    i += 2
                elif sql.startswith('*/', i):
                    depth -= 1
                    i += 2
                else:
                    i += 1
            if depth:
                raise ValueError('unterminated comment')
        elif c in "'\"":
            quote = c
            # E strings allow backslash escapes; normal strings do not.
            escaped = quote == "'" and i > 0 and sql[i - 1] in 'eE' and (i < 2 or not sql[i - 2].isalnum())
            i += 1
            while i < len(sql):
                if escaped and sql[i] == '\\':
                    i += 2
                elif sql[i] == quote:
                    i += 1
                    if i < len(sql) and sql[i] == quote:
                        i += 1
                    else:
                        break
                else:
                    i += 1
            else:
                raise ValueError('unterminated quoted value')
            result.append('quoted_identifier' if quote == '"' else 'literal_value')
        elif c == '$' and (match := _DOLLAR.match(sql, i)):
            end = sql.find(match.group(), match.end())
            if end < 0:
                raise ValueError('unterminated dollar string')
            i = end + len(match.group())
            result.append('literal_value')
        elif match := _WORD.match(sql, i):
            result.append(match.group().lower())
            i = match.end()
        else:
            result.append(c)
            i += 1
    depth = 0
    for token in result:
        depth += (token == '(') - (token == ')')
        if depth < 0:
            raise ValueError('unbalanced parentheses')
    if depth:
        raise ValueError('unbalanced parentheses')
    return result


def _create_object_type(statement: list[str]) -> str | None:
    """Return the object type named right after `CREATE` modifiers.

    Only `CREATE [OR REPLACE] [TEMP|TEMPORARY] <type>` counts. Keywords elsewhere
    in the statement (a column named `function`, an index on a `procedure`
    column) must not be mistaken for the object type.
    """
    if not statement or statement[0] != 'create':
        return None
    i = 1
    if statement[i:i + 2] == ['or', 'replace']:
        i += 2
    if i < len(statement) and statement[i] in ('temp', 'temporary'):
        i += 1
    return statement[i] if i < len(statement) else None


def has_opaque_procedural_body(parts: list[str]) -> bool:
    """Recognize unsupported procedural statements using lexical tokens.

    Dollar/single quoted bodies stay opaque; executable containers are blocked
    rather than claiming that their invisible body passed a semantic check.
    """
    statement: list[str] = []
    for token in [*parts, ';']:
        if token != ';':
            statement.append(token)
            continue
        if statement:
            if statement[0] == 'do':
                return True
            if _create_object_type(statement) in {'function', 'procedure'}:
                return True
            if statement[:2] == ['begin', 'atomic']:
                return True
        statement = []
    return False


def _statement_lead(parts: list[str], index: int) -> str | None:
    """Return the leading token of the statement containing `parts[index]`.

    Returns None when the position sits inside parentheses, where the enclosing
    statement cannot be identified from lexical tokens alone.
    """
    depth = 0
    for i in range(index - 1, -1, -1):
        token = parts[i]
        if token == ')':
            depth += 1
        elif token == '(':
            if depth == 0:
                return None
            depth -= 1
        elif token == ';' and depth == 0:
            return parts[i + 1] if i + 1 < index else None
    return parts[0] if parts else None


def unconditional_dml(parts: list[str], keyword: str) -> bool:
    for start, token in enumerate(parts):
        if token != keyword:
            continue
        if keyword == 'update' and start and parts[start - 1] in {'for', 'key', 'do'}:
            # Row locks (FOR [NO KEY] UPDATE) and ON CONFLICT DO UPDATE are not
            # unrestricted UPDATE statements.
            continue
        if keyword == 'update' and _statement_lead(parts, start) == 'merge':
            # MERGE ... WHEN MATCHED THEN UPDATE is constrained by the ON clause
            # of the same statement, so it is not a full-table UPDATE. Updates
            # nested in parentheses return None above and stay reported.
            continue
        depth = 0
        has_where = False
        for following in parts[start + 1:]:
            if following == '(':
                depth += 1
            elif following == ')':
                if depth == 0:
                    break
                depth -= 1
            elif depth == 0 and following == ';':
                break
            elif depth == 0 and following == 'where':
                has_where = True
        if not has_where:
            return True
    return False
