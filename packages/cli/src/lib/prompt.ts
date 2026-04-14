import { createInterface } from 'node:readline/promises'
import { stdin, stdout } from 'node:process'

export async function askPort(defaultPort: number): Promise<number> {
  const rl = createInterface({ input: stdin, output: stdout })
  try {
    const answer = await rl.question(`Port [${defaultPort}]: `)
    const port = answer.trim() ? Number(answer.trim()) : defaultPort
    if (isNaN(port) || port < 1 || port > 65535) {
      console.error('Invalid port number.')
      return askPort(defaultPort)
    }
    return port
  } finally {
    rl.close()
  }
}

export async function askPassword(): Promise<string> {
  const password = await readHidden('Password (min 8 chars): ')
  if (password.length < 8) {
    console.error('Password must be at least 8 characters.')
    return askPassword()
  }
  const confirm = await readHidden('Confirm password: ')
  if (password !== confirm) {
    console.error('Passwords do not match.')
    return askPassword()
  }
  return password
}

async function readHidden(prompt: string): Promise<string> {
  return new Promise((resolve) => {
    const rl = createInterface({ input: stdin, output: stdout })
    // Disable echo for password input
    if (stdin.isTTY) stdin.setRawMode(true)
    stdout.write(prompt)

    let input = ''
    const onData = (ch: Buffer) => {
      const c = ch.toString()
      if (c === '\n' || c === '\r') {
        if (stdin.isTTY) stdin.setRawMode(false)
        stdin.removeListener('data', onData)
        stdout.write('\n')
        rl.close()
        resolve(input)
      } else if (c === '\x7f' || c === '\b') {
        // Backspace
        if (input.length > 0) {
          input = input.slice(0, -1)
          stdout.write('\b \b')
        }
      } else if (c === '\x03') {
        // Ctrl+C
        if (stdin.isTTY) stdin.setRawMode(false)
        rl.close()
        process.exit(1)
      } else {
        input += c
        stdout.write('*')
      }
    }
    stdin.on('data', onData)
  })
}

export async function readPasswordStdin(): Promise<string> {
  return new Promise((resolve, reject) => {
    let data = ''
    stdin.setEncoding('utf-8')
    stdin.on('data', (chunk) => { data += chunk })
    stdin.on('end', () => resolve(data.trim()))
    stdin.on('error', reject)
  })
}
