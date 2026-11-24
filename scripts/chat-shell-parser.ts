type Segment = { words: string[]; background: boolean };

// Only command-position executables count, never an echo/printf argument or quoted example.
export function parseCommands(command: string): Segment[] | null {
  const commands: Segment[] = [{ words: [], background: false }];
  let word = '';
  let started = false;
  let quote = '';
  const pendingHereDocs: { delimiter: string; stripTabs: boolean }[] = [];
  const flush = () => {
    if (started) commands.at(-1)!.words.push(word);
    word = '';
    started = false;
  };
  for (let i = 0; i < command.length; i++) {
    const c = command[i];
    if (
      !quote &&
      c === '<' &&
      command[i + 1] === '<' &&
      command[i - 1] !== '<' &&
      command[i + 2] !== '<'
    ) {
      flush();
      const stripTabs = command[i + 2] === '-';
      let at = i + (stripTabs ? 3 : 2);
      while (/[ \t]/.test(command[at] ?? '')) at++;
      let delimiter = '';
      let delimiterStarted = false;
      let delimiterQuote = '';
      while (at < command.length && command[at] !== '\n') {
        const next = command[at];
        if (!delimiterQuote && /[ \t;|&<>]/.test(next)) break;
        if (next === '\\' && delimiterQuote !== "'") {
          if (++at >= command.length || command[at] === '\n') return null;
          delimiter += command[at];
        } else if (delimiterQuote) {
          if (next === delimiterQuote) delimiterQuote = '';
          else delimiter += next;
        } else if (next === "'" || next === '"') {
          delimiterQuote = next;
        } else delimiter += next;
        at++;
        delimiterStarted = true;
      }
      if (!delimiterStarted || delimiterQuote) return null;
      pendingHereDocs.push({ delimiter, stripTabs });
      i = at - 1;
      continue;
    }
    if (c === '\\' && quote !== "'") {
      if (quote === '"') {
        const next = command[i + 1];
        if (next === undefined) return null;
        if (/[\\$`"\n]/.test(next)) {
          i++;
          if (next !== '\n') word += next;
        } else word += c;
        started = true;
      } else {
        if (++i >= command.length) return null;
        if (command[i] !== '\n') {
          word += command[i];
          started = true;
        }
      }
    } else if (quote) {
      if (c === quote) quote = '';
      else word += c;
    } else if (c === "'" || c === '"') {
      quote = c;
      started = true;
    } else if (c === '#' && !started) {
      while (i + 1 < command.length && command[i + 1] !== '\n') i++;
      flush();
      commands.push({ words: [], background: false });
    } else if (/[;&|\n]/.test(c)) {
      flush();
      // A lone & (not &&, |&, >&, <& or &>) sends the command before it to the background.
      if (
        c === '&' &&
        !/[&|<>]/.test(command[i - 1] ?? '') &&
        !/[&>]/.test(command[i + 1] ?? '')
      )
        commands.at(-1)!.background = true;
      commands.push({ words: [], background: false });
      if (c === '\n' && pendingHereDocs.length) {
        for (const { delimiter, stripTabs } of pendingHereDocs) {
          let bodyStart = i + 1;
          let found = false;
          while (bodyStart <= command.length) {
            const lineEnd = command.indexOf('\n', bodyStart);
            const end = lineEnd < 0 ? command.length : lineEnd;
            const line = command.slice(bodyStart, end).replace(/\r$/, '');
            if ((stripTabs ? line.replace(/^\t+/, '') : line) === delimiter) {
              i = end;
              found = true;
              break;
            }
            if (lineEnd < 0) break;
            bodyStart = lineEnd + 1;
          }
          if (!found) return null;
        }
        pendingHereDocs.length = 0;
      }
    } else if (/\s/.test(c)) flush();
    else {
      word += c;
      started = true;
    }
  }
  if (quote) return null;
  flush();
  return commands;
}
