/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { useEffect, useRef, useState } from 'react'

import './pixel-home.css'

type LineType =
  | 'title'
  | 'blank'
  | 'comment'
  | 'command'
  | 'output'
  | 'content'
  | 'success'

interface TerminalLine {
  type: LineType
  text: string
  icon?: string
  delay?: number
  showDivider?: boolean
}

interface DisplayLine {
  type: LineType
  text: string
  icon: string | null
  showDivider: boolean
  isComplete: boolean
}

const terminalLines: TerminalLine[] = [
  {
    type: 'title',
    text: 'New Quest: Connect to IINA.AI',
    icon: '/Dwarf_Scroll_IV.png',
    delay: 800,
    showDivider: true,
  },
  { type: 'blank', text: '', delay: 200 },
  { type: 'comment', text: '# Step 1 - Install Claude Code', delay: 600 },
  {
    type: 'command',
    text: 'curl -fsSL https://claude.ai/install.sh | bash',
    delay: 1000,
  },
  { type: 'output', text: 'Installation complete!', delay: 800 },
  { type: 'blank', text: '', delay: 400 },
  { type: 'comment', text: '# Step 2 - Configure API Gateway', delay: 600 },
  { type: 'command', text: "cat > ~/.claude/settings.json << 'EOF'", delay: 200 },
  { type: 'content', text: '{', delay: 100 },
  { type: 'content', text: '  "env": {', delay: 100 },
  { type: 'content', text: '    "ANTHROPIC_AUTH_TOKEN": "YOUR-KEY",', delay: 100 },
  { type: 'content', text: '    "ANTHROPIC_BASE_URL": "https://iina.ai"', delay: 100 },
  { type: 'content', text: '  }', delay: 100 },
  { type: 'content', text: '}', delay: 100 },
  { type: 'content', text: 'EOF', delay: 800 },
  { type: 'blank', text: '', delay: 400 },
  { type: 'comment', text: '# Step 3 - Start your adventure', delay: 600 },
  { type: 'command', text: 'claude', delay: 800 },
  { type: 'blank', text: '', delay: 300 },
  { type: 'success', text: 'Quest Complete! Happy coding!', delay: 1500 },
]

interface CodeTerminalProps {
  lines: TerminalLine[]
  speed?: number
  initialDelay?: number
  loop?: boolean
  loopDelay?: number
}

function CodeTerminal({
  lines,
  speed = 35,
  initialDelay = 500,
  loop = true,
  loopDelay = 4000,
}: CodeTerminalProps) {
  const [displayLines, setDisplayLines] = useState<DisplayLine[]>([])
  const [showCursor, setShowCursor] = useState(true)
  const containerRef = useRef<HTMLDivElement>(null)
  const animationRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    let isMounted = true

    const scrollToBottom = () => {
      if (containerRef.current) {
        containerRef.current.scrollTop = containerRef.current.scrollHeight
      }
    }

    const sleep = (ms: number) =>
      new Promise<void>((resolve) => {
        animationRef.current = setTimeout(() => resolve(), ms)
      })

    const runAnimation = async () => {
      while (isMounted) {
        // 重置
        setDisplayLines([])
        setShowCursor(true)
        await sleep(initialDelay)

        for (let lineIdx = 0; lineIdx < lines.length && isMounted; lineIdx++) {
          const line = lines[lineIdx]
          const isInstant = [
            'output',
            'content',
            'blank',
            'title',
            'success',
          ].includes(line.type)

          if (isInstant) {
            // 瞬间显示
            setDisplayLines((prev) => [
              ...prev,
              {
                type: line.type,
                text: line.text,
                icon: line.icon || null,
                showDivider: line.showDivider || false,
                isComplete: true,
              },
            ])
            scrollToBottom()
            await sleep(line.delay ?? 400)
          } else {
            // 打字效果
            // 先添加空行
            setDisplayLines((prev) => [
              ...prev,
              {
                type: line.type,
                text: '',
                icon: line.icon || null,
                showDivider: line.showDivider || false,
                isComplete: false,
              },
            ])
            scrollToBottom()

            // 逐字显示
            for (
              let charIdx = 0;
              charIdx <= line.text.length && isMounted;
              charIdx++
            ) {
              const currentText = line.text.substring(0, charIdx)
              setDisplayLines((prev) => {
                const newLines = [...prev]
                const last = newLines[newLines.length - 1]
                if (last) {
                  newLines[newLines.length - 1] = {
                    ...last,
                    text: currentText,
                    isComplete: charIdx === line.text.length,
                  }
                }
                return newLines
              })
              if (charIdx < line.text.length) {
                await sleep(speed)
              }
            }
            await sleep(line.delay ?? 600)
          }
        }

        // 动画完成
        setShowCursor(false)

        if (!loop || !isMounted) break
        await sleep(loopDelay)
      }
    }

    void runAnimation()

    return () => {
      isMounted = false
      if (animationRef.current) {
        clearTimeout(animationRef.current)
      }
    }
  }, [lines, speed, initialDelay, loop, loopDelay])

  const getLineClassName = (type: LineType): string => {
    switch (type) {
      case 'command':
        return 'text-slate-800'
      case 'comment':
        return 'text-slate-500 italic'
      case 'output':
        return 'text-green-700'
      case 'content':
        return 'text-blue-800'
      case 'title':
        return 'text-amber-700 font-bold text-xl'
      case 'success':
        return 'text-green-600 font-bold'
      default:
        return ''
    }
  }

  const isQuestComplete =
    displayLines.length > 0 &&
    displayLines[displayLines.length - 1]?.type === 'success' &&
    displayLines[displayLines.length - 1]?.isComplete

  return (
    <div
      ref={containerRef}
      className='paper-content relative h-[400px] overflow-y-auto scroll-smooth p-4 pt-6 text-lg leading-7 text-slate-800'
    >
      <div className='font-pixel min-h-full w-full pb-8'>
        {displayLines.map((line, index) => (
          <div key={index} className='break-all whitespace-pre-wrap'>
            {line.type === 'command' && (
              <span className='font-bold text-purple-700 select-none'>$ </span>
            )}
            {line.icon && (
              <img
                src={line.icon}
                alt=''
                className='mr-2 -mt-1 inline-block h-8 w-8 align-middle'
                style={{ imageRendering: 'pixelated' }}
              />
            )}
            <span className={getLineClassName(line.type)}>{line.text}</span>
            {!line.isComplete &&
              index === displayLines.length - 1 &&
              showCursor && (
                <span className='cursor-blink ml-1 inline-block h-5 w-2 bg-slate-800 align-middle'></span>
              )}
            {line.showDivider && line.isComplete && (
              <div className='mt-2 h-px w-full bg-amber-600/40'></div>
            )}
          </div>
        ))}
        {displayLines.length > 0 &&
          !isQuestComplete &&
          showCursor &&
          displayLines[displayLines.length - 1]?.isComplete && (
            <div className='mt-1'>
              <span className='font-bold text-purple-700'>$ </span>
              <span className='cursor-blink inline-block h-5 w-2 bg-slate-800 align-middle'></span>
            </div>
          )}
      </div>
    </div>
  )
}

export function PixelHome() {
  return (
    <div className='sdv-home flex min-h-screen items-center justify-center p-4 pt-20 lg:pt-4'>
      <div className='w-full max-w-6xl'>
        <div className='grid grid-cols-1 items-center gap-6 lg:grid-cols-2'>
          {/* 左侧：标题与说明 */}
          <div className='flex flex-col justify-center space-y-6 text-center lg:py-8 lg:text-left'>
            <div className='relative inline-block'>
              <h1 className='logo-text font-pixel mb-2 text-6xl leading-none lg:text-7xl'>
                The Unified
                <br />
                LLMs API Gateway
              </h1>
              <div className='absolute -top-4 -right-4 animate-pulse text-4xl text-yellow-400'>
                ✦
              </div>
            </div>

            <p className='font-pixel text-sdv-brown-dark text-2xl leading-relaxed lg:pr-12'>
              The valley's finest connection point.
              <br />
              <span className='text-sdv-ui-border opacity-80'>
                One interface to rule all models.
              </span>
            </p>

            {/* 四季树木装饰 */}
            <div className='mt-8 flex justify-center lg:justify-start'>
              <img
                src='/trees.png'
                alt='Stardew Valley Trees'
                className='h-auto w-72 sm:w-80 lg:w-[400px]'
                style={{ imageRendering: 'pixelated' }}
              />
            </div>
          </div>

          {/* 右侧：告示板终端 */}
          <div className='terminal-wrapper relative'>
            {/* 小鸡装饰 - 告示板左下角外侧 */}
            <div className='absolute -bottom-4 -left-10 z-30 hidden lg:block'>
              <svg
                width='36'
                height='36'
                viewBox='0 0 16 16'
                fill='#fff'
                xmlns='http://www.w3.org/2000/svg'
                className='drop-shadow-md'
              >
                <path
                  d='M5 2H9V3H10V4H11V6H12V7H13V10H14V12H13V13H11V14H6V13H4V12H3V11H2V10H3V9H2V7H3V6H4V4H5V2ZM5 3V4H4V6H3V7H4V9H5V10H4V11H5V12H6V13H11V12H12V10H13V8H12V7H11V6H10V5H9V3H5Z'
                  fill='#e09c52'
                />
                <path d='M11 6H12V7H11V6ZM10 4H11V6H10V4Z' fill='#d05040' />
                <path d='M12 8H14V9H13V10H12V8Z' fill='#ffce31' />
                <path d='M7 6H8V7H7V6Z' fill='#333' />
              </svg>
            </div>

            <div className='sdv-panel rounded-sm p-6'>
              {/* 四角木质铆钉 */}
              <div className='wood-corner' style={{ top: '6px', left: '6px' }}></div>
              <div className='wood-corner' style={{ top: '6px', right: '6px' }}></div>
              <div
                className='wood-corner'
                style={{ bottom: '6px', left: '6px' }}
              ></div>
              <div
                className='wood-corner'
                style={{ bottom: '6px', right: '6px' }}
              ></div>

              <div className='border-sdv-ui-border/30 mb-3 flex items-center justify-between border-b-2 pb-2'>
                <span className='font-pixel text-sdv-brown-dark text-2xl tracking-widest uppercase'>
                  Notice Board
                </span>
              </div>

              <div className='relative'>
                <div className='quest-board p-1'>
                  <CodeTerminal
                    lines={terminalLines}
                    speed={35}
                    loop={true}
                    loopDelay={4000}
                  />
                </div>
              </div>

              <div className='mt-2 text-center'>
                <span className='font-pixel text-sdv-brown-dark text-lg opacity-60'>
                  - IINA's General Store -
                </span>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}
